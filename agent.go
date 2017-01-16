package gocbcore

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"
)

// This class represents the base client handling connections to a Couchbase Server.
// This is used internally by the higher level classes for communicating with the cluster,
// it can also be used to perform more advanced operations with a cluster.
type Agent struct {
	bucket            string
	password          string
	tlsConfig         *tls.Config
	initFn            memdInitFunc
	useMutationTokens bool

	configLock  sync.Mutex
	routingInfo routeDataPtr
	numVbuckets int

	serverFailuresLock sync.Mutex
	serverFailures     map[string]time.Time

	httpCli *http.Client

	serverConnectTimeout time.Duration
	serverWaitTimeout    time.Duration
	nmvRetryDelay        time.Duration
	kvPoolSize           int
	maxQueueSize         int
}

// The timeout for each server connection, including all authentication steps.
func (agent *Agent) ServerConnectTimeout() time.Duration {
	return agent.serverConnectTimeout
}

// Sets the timeout for each server connection.
func (agent *Agent) SetServerConnectTimeout(timeout time.Duration) {
	agent.serverConnectTimeout = timeout
}

// Returns a pre-configured HTTP Client for communicating with
// Couchbase Server.  You must still specify authentication
// information for any dispatched requests.
func (agent *Agent) HttpClient() *http.Client {
	return agent.httpCli
}

type AuthFunc func(client AuthClient, deadline time.Time) error

type AgentConfig struct {
	MemdAddrs         []string
	HttpAddrs         []string
	TlsConfig         *tls.Config
	BucketName        string
	Password          string
	AuthHandler       AuthFunc
	UseMutationTokens bool

	ConnectTimeout       time.Duration
	ServerConnectTimeout time.Duration
	NmvRetryDelay        time.Duration
	MaxQueueSize         int
}

func createInitFn(config *AgentConfig) memdInitFunc {
	return func(client *syncClient, deadline time.Time) error {
		var features []HelloFeature
		if config.UseMutationTokens {
			features = append(features, FeatureSeqNo)
		}

		client.ExecHello(features, deadline)

		return config.AuthHandler(client, deadline)
	}
}

// Creates an agent for performing normal operations.
func CreateAgent(config *AgentConfig) (*Agent, error) {
	initFn := createInitFn(config)
	return createAgent(config, initFn)
}

// **INTERNAL**
// Creates an agent for performing DCP operations.
func CreateDcpAgent(config *AgentConfig, dcpStreamName string) (*Agent, error) {
	// We wrap the authorization system to force DCP channel opening
	//   as part of the "initialization" for any servers.
	initFn := createInitFn(config)
	dcpInitFn := func(client *syncClient, deadline time.Time) error {
		if err := initFn(client, deadline); err != nil {
			return err
		}
		return client.ExecOpenDcpConsumer(dcpStreamName, deadline)
	}
	return createAgent(config, dcpInitFn)
}

func createAgent(config *AgentConfig, initFn memdInitFunc) (*Agent, error) {
	// TODO(brett19): Put all configurable options in the AgentConfig

	c := &Agent{
		bucket:    config.BucketName,
		password:  config.Password,
		tlsConfig: config.TlsConfig,
		initFn:    initFn,
		httpCli: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: config.TlsConfig,
			},
		},
		useMutationTokens:    config.UseMutationTokens,
		serverFailures:       make(map[string]time.Time),
		serverConnectTimeout: config.ServerConnectTimeout,
		serverWaitTimeout:    5 * time.Second,
		nmvRetryDelay:        config.NmvRetryDelay,
		kvPoolSize:           1,
		maxQueueSize:         2048,
	}

	deadline := time.Now().Add(config.ConnectTimeout)
	if err := c.connect(config.MemdAddrs, config.HttpAddrs, deadline); err != nil {
		return nil, err
	}
	return c, nil
}

func (agent *Agent) cccpLooper() {
	tickTime := time.Second * 10
	maxWaitTime := time.Second * 3

	logDebugf("CCCP Looper starting.")

	for {
		// Wait 10 seconds
		time.Sleep(tickTime)

		routingInfo := agent.routingInfo.get()
		if routingInfo == nil {
			// If we have a blank routingInfo, it indicates the client is shut down.
			break
		}

		numNodes := len(routingInfo.kvPipelines)
		if numNodes == 0 {
			logDebugf("CCCPPOLL: No nodes available to poll")
			continue
		}

		nodeIdx := rand.Intn(numNodes)
		pipeline := routingInfo.kvPipelines[nodeIdx]

		client := syncClient{
			client: pipeline,
		}
		cccpBytes, err := client.ExecCccpRequest(time.Now().Add(maxWaitTime))
		if err != nil {
			logDebugf("CCCPPOLL: Failed to retrieve CCCP config. %v", err)
			continue
		}

		hostName, _, _ := net.SplitHostPort(pipeline.Address())
		bk, err := parseConfig(cccpBytes, hostName)
		if err != nil {
			logDebugf("CCCPPOLL: Failed to parse CCCP config. %v", err)
			continue
		}

		logDebugf("CCCPPOLL: Received new config")
		agent.updateConfig(bk)
	}
}

func (agent *Agent) connect(memdAddrs, httpAddrs []string, deadline time.Time) error {
	logDebugf("Attempting to connect...")

	for _, thisHostPort := range memdAddrs {
		logDebugf("Trying server at %s", thisHostPort)

		srvDeadlineTm := time.Now().Add(agent.serverConnectTimeout)
		if srvDeadlineTm.After(deadline) {
			srvDeadlineTm = deadline
		}

		logDebugf("Trying to connect")
		client, err := agent.dialMemdClient(thisHostPort)
		if err == ErrAuthError {
			return err
		} else if err != nil {
			logDebugf("Connecting failed! %v", err)
			continue
		}

		syncCli := syncClient{
			client: client,
		}

		logDebugf("Attempting to request CCCP configuration")
		cccpBytes, err := syncCli.ExecCccpRequest(srvDeadlineTm)
		if err != nil {
			logDebugf("Failed to retrieve CCCP config. %v", err)
			client.Close()
			continue
		}

		hostName, _, _ := net.SplitHostPort(thisHostPort)
		bk, err := parseConfig(cccpBytes, hostName)
		if err != nil {
			logDebugf("Failed to parse CCCP configuration")
			client.Close()
			continue
		}

		if !bk.supportsCccp() {
			logDebugf("Bucket does not support CCCP")
			client.Close()
			break
		}

		routeCfg := buildRouteConfig(bk, agent.IsSecure())
		if !routeCfg.IsValid() {
			logDebugf("Configuration was deemed invalid")
			client.Close()
			continue
		}

		logDebugf("Successfully connected")

		// Build some fake routing data, this is used to indicate that
		//  client is "alive".  A nil routeData causes immediate shutdown.
		agent.routingInfo.update(nil, &routeData{
			revId: -1,
		})

		// TODO(brett19): Save the client that we build for bootstrap
		client.Close()

		agent.numVbuckets = len(routeCfg.vbMap)
		agent.applyConfig(routeCfg)

		go agent.cccpLooper()

		return nil
	}

	signal := make(chan error, 1)

	var epList []string
	for _, hostPort := range httpAddrs {
		if !agent.IsSecure() {
			epList = append(epList, fmt.Sprintf("http://%s", hostPort))
		} else {
			epList = append(epList, fmt.Sprintf("https://%s", hostPort))
		}
	}
	agent.routingInfo.update(nil, &routeData{
		revId:      -1,
		mgmtEpList: epList,
	})

	var routeCfg *routeConfig

	logDebugf("Starting HTTP looper! %v", epList)
	go agent.httpLooper(func(cfg *cfgBucket, err error) bool {
		if err != nil {
			signal <- err
			return true
		}

		newRouteCfg := buildRouteConfig(cfg, agent.IsSecure())
		if !newRouteCfg.IsValid() {
			// Something is invalid about this config, keep trying
			return false
		}

		routeCfg = newRouteCfg
		signal <- nil
		return true
	})

	err := <-signal
	if err != nil {
		return err
	}

	agent.numVbuckets = len(routeCfg.vbMap)
	agent.applyConfig(routeCfg)

	return nil
}

// Shuts down the agent, disconnecting from all servers and failing
// any outstanding operations with ErrShutdown.
func (agent *Agent) Close() {
	// Clear the routingInfo so no new operations are performed
	//   and retrieve the last active routing configuration
	routingInfo := agent.routingInfo.clear()
	if routingInfo == nil {
		return
	}

	// Loop all the pipelines and close them, then close the wait
	//  queue to prevent any further data from entering them.
	for _, pipeline := range routingInfo.kvPipelines {
		pipeline.Close()
	}
	if routingInfo.deadPipe != nil {
		routingInfo.deadPipe.Close()
	}

	// Drain all the pipelines and error their requests, then
	//  drain the dead queue and error those requests.
	dispatchReqErr := func(req *memdQRequest) {
		req.tryCallback(nil, ErrShutdown)
	}
	for _, pipeline := range routingInfo.kvPipelines {
		pipeline.Drain(dispatchReqErr)
	}
	if routingInfo.deadPipe != nil {
		routingInfo.deadPipe.Drain(dispatchReqErr)
	}
}

// Returns whether this client is connected via SSL.
func (agent *Agent) IsSecure() bool {
	return agent.tlsConfig != nil
}

// Translates a particular key to its assigned vbucket.
func (agent *Agent) KeyToVbucket(key []byte) uint16 {
	if agent.NumVbuckets() <= 0 {
		return 0xFFFF
	}
	return uint16(cbCrc(key) % uint32(agent.NumVbuckets()))
}

// Returns the number of VBuckets configured on the
// connected cluster.
func (agent *Agent) NumVbuckets() int {
	return agent.numVbuckets
}

// Returns the number of replicas configured on the
// connected cluster.
func (agent *Agent) NumReplicas() int {
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return 0
	}
	return len(routingInfo.vbMap[0]) - 1
}

// Returns number of servers accessible for K/V.
func (agent *Agent) NumServers() int {
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return 0
	}
	return len(routingInfo.kvPipelines)
}

// Returns list of VBuckets on the server.
func (agent *Agent) VbucketsOnServer(index int) []uint16 {
	var vbuckets []uint16
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return vbuckets
	}

	for vb, entry := range routingInfo.vbMap {
		if entry[0] == index {
			vbuckets = append(vbuckets, uint16(vb))
		}
	}
	return vbuckets
}

// Returns all the available endpoints for performing
// map-reduce queries.
func (agent *Agent) CapiEps() []string {
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.capiEpList
}

// Returns all the available endpoints for performing
// management queries.
func (agent *Agent) MgmtEps() []string {
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.mgmtEpList
}

// Returns all the available endpoints for performing
// N1QL queries.
func (agent *Agent) N1qlEps() []string {
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.n1qlEpList
}

// Returns all the available endpoints for performing
// FTS queries.
func (agent *Agent) FtsEps() []string {
	routingInfo := agent.routingInfo.get()
	if routingInfo == nil {
		return nil
	}
	return routingInfo.ftsEpList
}
