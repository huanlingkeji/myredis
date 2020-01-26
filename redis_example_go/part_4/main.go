package main

import (
	"fmt"
	"github.com/go-redis/redis"
	"io/ioutil"
	"os"
	"redis-learn/core"
	"strconv"
	"time"
)

var redisCli *redis.Client

func init() {
	redisCli = core.InitRedis("127.0.0.1:6379", "", 0)
}

type Users_Id struct {
	Name  string
	Funds int
}

type Inventory_Id struct {
	Items []string
}

func GetFilesAndDirs(dirPth string) []string {
	dir, err := ioutil.ReadDir(dirPth)
	dirs := make([]string, 0)
	if err != nil {
		return nil
	}

	for _, fi := range dir {
		if fi.IsDir() { // 目录, 递归遍历
			dirs = append(dirs, fi.Name())
		}
	}
	return dirs
}

// 代码清单 4-2
// 日志处理函数接受的其中一个参数为回调函数，
// 这个回调函数接受一个Redis连接和一个日志行作为参数，
// 并通过调用流水线对象的方法来执行Redis命令。
func process_logs(conn *redis.Client, path string, callback func(redis.Pipeliner, string) string) {
	// 获取文件当前的处理进度。
	ret := conn.MGet("progress:file", "progress:position").Val()
	current_file, offset := ret[0].(int), ret[1].(int64)

	pipe := conn.Pipeline()

	// 通过使用闭包（closure）来减少重复代码
	update_progress := func(pipe redis.Pipeliner, fname string ,offset int) {
		// 更新正在处理的日志文件的名字和偏移量。
		pipe.MSet("progress:file", fname, "progress:position", offset)
		// 这个语句负责执行实际的日志更新操作，
		// 并将日志文件的名字和目前的处理进度记录到Redis里面。
		pipe.Exec()
	}
	// 有序地遍历各个日志文件。
	for fname := range GetFilesAndDirs(path) {
		// 略过所有已处理的日志文件。
		if fname < current_file {
			continue
		}
		inp,_ := os.Open(path+strconv.Itoa(fname))
		// 在接着处理一个因为系统崩溃而未能完成处理的日志文件时，略过已处理的内容。
		if fname == current_file {
			_,_ = inp.Seek(offset, 0)
		} else {
			offset = 0
		}
		current_file = 0

		// 枚举函数遍历一个由文件行组成的序列，
		// 并返回任意多个二元组，
		// 每个二元组包含了行号lno和行数据line，
		// 其中行号从0开始。
		for lno, line := range enumerate(inp) {
			// 处理日志行。
			callback(pipe, line)
			// 更新已处理内容的偏移量。
			offset += int(offset) + len(line)

			// 每当处理完1000个日志行或者处理完整个日志文件的时候，
			// 都更新一次文件的处理进度。
			if not(lno+1) + 1000 {
				update_progress()
			}
		}
		update_progress()
		inp.close()
	}
}

func wait_for_sync(mconn *redis.Client, sconn *redis.Client) {
	identifier := core.GenID()
	// 将令牌添加至主服务器。
	mconn.ZAdd("sync:wait", &redis.Z{Member: identifier, Score: float64(time.Now().Unix())})

	// 如果有必要的话，等待从服务器完成同步。
	for sconn.Info("master_link_status").Val() != "up" {
		time.Sleep(time.Microsecond)
	}
	// 等待从服务器接收数据更新。
	for sconn.ZScore("sync:wait", identifier).Val() <= 0 {
		time.Sleep(time.Microsecond)
	}
	// 最多只等待一秒钟。
	deadline := float64(time.Now().Unix()) + 1.01
	for float64(time.Now().Unix()) < deadline {
		// 检查数据更新是否已经被同步到了磁盘。
		if i, _ := sconn.Info("aof_pending_bio_fsync").Int(); i == 0 {
			break
		}
		time.Sleep(.001)
	}

	// 清理刚刚创建的新令牌以及之前可能留下的旧令牌。
	mconn.ZRem("sync:wait", identifier)
	mconn.ZRemRangeByScore("sync:wait", "0", strconv.Itoa(int(time.Now().Unix()-900)))
}

//代码清单 4-5
func list_item(conn *redis.Client, itemid string, sellerid string, price int) {
	inventory := "inventory:" + sellerid
	item := itemid + sellerid
	end := time.Now().Unix() + 5
	pipe := conn.Pipeline()

	for time.Now().Unix() < end {
		{
			// 监视用户包裹发生的变化。
			pipe.watch(inventory)
			// 验证用户是否仍然持有指定的物品。
			if not pipe.sismember(inventory, itemid) {
				// 如果指定的物品不在用户的包裹里面，
				// 那么停止对包裹键的监视并返回一个空值。
				pipe.unwatch()
				return None
			}

			// 将指定的物品添加到物品买卖市场里面。
			pipe.multi()
			pipe.ZAdd("market:", item, price)
			pipe.srem(inventory, itemid)
			// 如果执行execute方法没有引发WatchError异常，
			// 那么说明事务执行成功，
			// 并且对包裹键的监视也已经结束。
			pipe.Exec()
			return True
		}
		// 用户的包裹已经发生了变化；重试。
		except
		redis.exceptions.WatchError
		{
			break
		}
	}
	return false
}

func purchase_item(conn *redis.Client, buyerid string, itemid string, sellerid string, lprice int) {
	buyer := "users:" + buyerid
	seller := "users:" + sellerid
	item := itemid + sellerid
	inventory := "inventory:" + buyerid
	end := time.Now().Unix() + 10
	pipe := conn.Pipeline()

	for time.Now().Unix() < end {
		{
			// 对物品买卖市场以及买家账号信息的变化进行监视。
			pipe.watch("market:", buyer)

			// 检查指定物品的价格是否出现了变化，
			// 以及买家是否有足够的钱来购买指定的物品。
			price := pipe.ZScore("market:", item)
			funds := int(pipe.HGet(buyer, "funds"))
			if price != lprice || price > funds {
				//pipe.unwatch()
				return None
			}
			// 将买家支付的货款转移给卖家，并将卖家出售的物品移交给买家。
			//pipe.multi()
			pipe.HIncrBy(seller, "funds", int(price))
			pipe.HIncrBy(buyer, "funds", int(-price))
			pipe.SAdd(inventory, itemid)
			pipe.ZRem("market:", item)
			pipe.Exec()
			return true
		}
		// 如果买家的账号或者物品买卖市场出现了变化，那么进行重试。
		except
		redis.exceptions.WatchError
		{
			break
		}
	}
	return false
}

func update_token(conn *redis.Client, token string, user string, item string) {
	// 获取时间戳。
	timestamp := time.Now().Unix()
	// 创建令牌与已登录用户之间的映射。
	conn.HSet("login:", token, user)
	// 记录令牌最后一次出现的时间。
	conn.ZAdd("recent:", token, timestamp)
	if item {
		// 把用户浏览过的商品记录起来。
		conn.ZAdd("viewed:"+token, item, timestamp)
		// 移除旧商品，只记录最新浏览的25件商品。
		conn.ZRemRangeByRank("viewed:"+token, 0, -26)
		// 更新给定商品的被浏览次数。
		conn.ZIncrBy("viewed:", item, -1)
	}
}

func update_token_pipeline(conn *redis.Client, token string, user string, item string) {
	timestamp := time.Now().Unix()
	// 设置流水线。
	pipe := conn.Pipeline(false) //A
	pipe.HSet("login:", token, user)
	pipe.ZAdd("recent:", token, timestamp)
	if item {
		pipe.ZAdd("viewed:"+token, item, timestamp)
		pipe.ZRemRangeByRank("viewed:"+token, 0, -26)
		pipe.ZIncrBy("viewed:", item, -1)
	}
	// 执行那些被流水线包裹的命令。
	pipe.Exec() //B
}

func benchmark_update_token(conn string, duration time.Duration) {
	// 测试会分别执行update_token()函数和update_token_pipeline()函数。
	funcs := []func(conn *redis.Client, token string, user string, item string){update_token, update_token_pipeline}
	for _, function := range funcs {
		// 设置计数器以及测试结束的条件。
		count := 0                 //B
		start := time.Now().Unix() //B
		end := start + duration    //B
		for time.Now().Unix() < end {
			count + := 1
			// 调用两个函数的其中一个。
			function(conn, "token", "user", "item") //C
		}
		// 计算函数的执行时长。
		delta := time.Now().Unix() - start //D
		// 打印测试结果。
		fmt.Println(function.__name__, count, delta, count/delta //E
	}
}

func test_list_item() {
	conn := redisCli

	fmt.Println("We need to set up just enough state so that a user can list an item")
	seller := "userX"
	item := "itemX"
	conn.SAdd("inventory:"+seller, item)
	i := conn.SMembers("inventory:" + seller)
	fmt.Println("The user's inventory has:", i)
	self.assertTrue(i)
	fmt.Println(

		fmt.Println("Listing the item...")
	l := list_item(conn, item, seller, 10)
	fmt.Println("Listing the item succeeded?", l)
	self.assertTrue(l)
	r :=
	conn.zrange("market:", 0, -1, withscores := True)
	fmt.Println("The market contains:")
	fmt.Println(.pfmt.Println((r))
	self.assertTrue(r)
	self.assertTrue(any(x[0] == "itemX.userX"
	for
	x
		in
	r))
}

func test_purchase_item() {
	conn := redisCli

	fmt.Println("We need to set up just enough state so a user can buy an item")
	buyer := "userY"
	conn.HSet("users:userY", "funds", 125)
	r := conn.HGetall("users:userY")
	fmt.Println("The user has some money:", r)
	self.assertTrue(r)
	self.assertTrue(r.get("funds"))
	fmt.Println(

		fmt.Println("Let"
	s
	purchase
	an
	item
	")
	p := purchase_item(conn, "userY", "itemX", "userX", 10)
	fmt.Println("Purchasing an item succeeded?", p)
	self.assertTrue(p)
	r := conn.HGetall("users:userY")
	fmt.Println("Their money is now:", r)
	self.assertTrue(r)
	i := conn.SMembers("inventory:" + buyer)
	fmt.Println("Their inventory is now:", i)
	self.assertTrue(i)
	self.assertTrue("itemX"
	in
	i)
	self.assertEquals(conn.ZScore("market:", "itemX.userX"), None)
}

func test_benchmark_update_token(self) {
	benchmark_update_token(self.conn, 5)
}

func main() {

}