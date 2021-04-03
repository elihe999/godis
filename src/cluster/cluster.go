package cluster

import (
	"context"
	"fmt"
	"github.com/HDT3213/godis/src/cluster/idgenerator"
	"github.com/HDT3213/godis/src/config"
	"github.com/HDT3213/godis/src/datastruct/dict"
	"github.com/HDT3213/godis/src/db"
	"github.com/HDT3213/godis/src/interface/redis"
	"github.com/HDT3213/godis/src/lib/consistenthash"
	"github.com/HDT3213/godis/src/lib/logger"
	"github.com/HDT3213/godis/src/redis/reply"
	"github.com/jolestar/go-commons-pool/v2"
	"runtime/debug"
	"strings"
)

type Cluster struct {
	self string

	peerPicker     *consistenthash.Map
	peerConnection map[string]*pool.ObjectPool

	db           *db.DB
	transactions *dict.SimpleDict // id -> Transaction

	idGenerator *idgenerator.IdGenerator
}

const (
	replicas = 4
	lockSize = 64
)

func MakeCluster() *Cluster {
	cluster := &Cluster{
		self: config.Properties.Self,

		db:             db.MakeDB(),
		transactions:   dict.MakeSimple(),
		peerPicker:     consistenthash.New(replicas, nil),
		peerConnection: make(map[string]*pool.ObjectPool),

		idGenerator: idgenerator.MakeGenerator("godis", config.Properties.Self),
	}
	if config.Properties.Peers != nil && len(config.Properties.Peers) > 0 && config.Properties.Self != "" {
		contains := make(map[string]bool)
		peers := make([]string, 0, len(config.Properties.Peers)+1)
		for _, peer := range config.Properties.Peers {
			if _, ok := contains[peer]; ok {
				continue
			}
			contains[peer] = true
			peers = append(peers, peer)
		}
		peers = append(peers, config.Properties.Self)
		cluster.peerPicker.Add(peers...)
		ctx := context.Background()
		for _, peer := range peers {
			cluster.peerConnection[peer] = pool.NewObjectPoolWithDefaultConfig(ctx, &ConnectionFactory{
				Peer: peer,
			})
		}
	}
	return cluster
}

// args contains all
type CmdFunc func(cluster *Cluster, c redis.Connection, args [][]byte) redis.Reply

func (cluster *Cluster) Close() {
	cluster.db.Close()
}

var router = MakeRouter()

func (cluster *Cluster) Exec(c redis.Connection, args [][]byte) (result redis.Reply) {
	defer func() {
		if err := recover(); err != nil {
			logger.Warn(fmt.Sprintf("error occurs: %v\n%s", err, string(debug.Stack())))
			result = &reply.UnknownErrReply{}
		}
	}()

	cmd := strings.ToLower(string(args[0]))
	cmdFunc, ok := router[cmd]
	if !ok {
		return reply.MakeErrReply("ERR unknown command '" + cmd + "', or not supported in cluster mode")
	}
	result = cmdFunc(cluster, c, args)
	return
}

func (cluster *Cluster) AfterClientClose(c redis.Connection) {

}

func Ping(cluster *Cluster, c redis.Connection, args [][]byte) redis.Reply {
	if len(args) == 1 {
		return &reply.PongReply{}
	} else if len(args) == 2 {
		return reply.MakeStatusReply("\"" + string(args[1]) + "\"")
	} else {
		return reply.MakeErrReply("ERR wrong number of arguments for 'ping' command")
	}
}

/*----- utils -------*/

func makeArgs(cmd string, args ...string) [][]byte {
	result := make([][]byte, len(args)+1)
	result[0] = []byte(cmd)
	for i, arg := range args {
		result[i+1] = []byte(arg)
	}
	return result
}

// return peer -> keys
func (cluster *Cluster) groupBy(keys []string) map[string][]string {
	result := make(map[string][]string)
	for _, key := range keys {
		peer := cluster.peerPicker.Get(key)
		group, ok := result[peer]
		if !ok {
			group = make([]string, 0)
		}
		group = append(group, key)
		result[peer] = group
	}
	return result
}