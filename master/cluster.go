// Copyright 2018 The Chubao Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/raftstore"
	"github.com/chubaofs/chubaofs/util"
	"github.com/chubaofs/chubaofs/util/errors"
	"github.com/chubaofs/chubaofs/util/log"
)

// Cluster stores all the cluster-level information.
type Cluster struct {
	Name                      string
	vols                      map[string]*Vol
	dataNodes                 sync.Map
	metaNodes                 sync.Map
	dpMutex                   sync.Mutex   // data partition mutex
	volMutex                  sync.RWMutex // volume mutex
	createVolMutex            sync.RWMutex // create volume mutex
	mnMutex                   sync.RWMutex // meta node mutex
	dnMutex                   sync.RWMutex // data node mutex
	leaderInfo                *LeaderInfo
	cfg                       *clusterConfig
	retainLogs                uint64
	idAlloc                   *IDAllocator
	t                         *topology
	dataNodeStatInfo          *nodeStatInfo
	metaNodeStatInfo          *nodeStatInfo
	zoneStatInfos             map[string]*proto.ZoneStat
	volStatInfo               sync.Map
	BadDataPartitionIds       *sync.Map
	BadMetaPartitionIds       *sync.Map
	MigratedMetaPartitionIds  *sync.Map
	MigratedDataPartitionIds  *sync.Map
	DisableAutoAllocate       bool
	fsm                       *MetadataFsm
	partition                 raftstore.Partition
	MasterSecretKey           []byte
	lastMasterZoneForDataNode string
	lastMasterZoneForMetaNode string
}

func newCluster(name string, leaderInfo *LeaderInfo, fsm *MetadataFsm, partition raftstore.Partition, cfg *clusterConfig) (c *Cluster) {
	c = new(Cluster)
	c.Name = name
	c.leaderInfo = leaderInfo
	c.vols = make(map[string]*Vol, 0)
	c.cfg = cfg
	c.t = newTopology()
	c.BadDataPartitionIds = new(sync.Map)
	c.BadMetaPartitionIds = new(sync.Map)
	c.MigratedDataPartitionIds = new(sync.Map)
	c.MigratedMetaPartitionIds = new(sync.Map)
	c.dataNodeStatInfo = new(nodeStatInfo)
	c.metaNodeStatInfo = new(nodeStatInfo)
	c.zoneStatInfos = make(map[string]*proto.ZoneStat)
	c.fsm = fsm
	c.partition = partition
	c.idAlloc = newIDAllocator(c.fsm.store, c.partition)
	return
}

func (c *Cluster) scheduleTask() {
	c.scheduleToCheckDataPartitions()
	c.scheduleToLoadDataPartitions()
	c.scheduleToCheckReleaseDataPartitions()
	c.scheduleToCheckHeartbeat()
	c.scheduleToCheckMetaPartitions()
	c.scheduleToUpdateStatInfo()
	c.scheduleToCheckAutoDataPartitionCreation()
	c.scheduleToCheckVolStatus()
	c.scheduleToCheckDiskRecoveryProgress()
	c.scheduleToCheckMetaPartitionRecoveryProgress()
	c.scheduleToLoadMetaPartitions()
	c.scheduleToReduceReplicaNum()
}

func (c *Cluster) masterAddr() (addr string) {
	return c.leaderInfo.addr
}

func (c *Cluster) scheduleToUpdateStatInfo() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.updateStatInfo()
			}
			time.Sleep(2 * time.Minute)
		}
	}()

}

func (c *Cluster) scheduleToCheckAutoDataPartitionCreation() {
	go func() {

		// check volumes after switching leader two minutes
		time.Sleep(2 * time.Minute)
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				vols := c.copyVols()
				for _, vol := range vols {
					vol.checkAutoDataPartitionCreation(c)
				}
			}
			time.Sleep(2 * time.Minute)
		}
	}()
}

func (c *Cluster) scheduleToCheckDataPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.checkDataPartitions()
			}
			time.Sleep(time.Second * time.Duration(c.cfg.IntervalToCheckDataPartition))
		}
	}()
}

func (c *Cluster) scheduleToCheckVolStatus() {
	go func() {
		//check vols after switching leader two minutes
		for {
			if c.partition.IsRaftLeader() {
				vols := c.copyVols()
				for _, vol := range vols {
					vol.checkStatus(c)
				}
			}
			time.Sleep(time.Second * time.Duration(c.cfg.IntervalToCheckDataPartition))
		}
	}()
}

// Check the replica status of each data partition.
func (c *Cluster) checkDataPartitions() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("checkDataPartitions occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"checkDataPartitions occurred panic")
		}
	}()

	vols := c.allVols()
	for _, vol := range vols {
		readWrites := vol.checkDataPartitions(c)
		vol.dataPartitions.setReadWriteDataPartitions(readWrites, c.Name)
		vol.dataPartitions.updateResponseCache(true, 0)
		msg := fmt.Sprintf("action[checkDataPartitions],vol[%v] can readWrite partitions:%v  ", vol.Name, vol.dataPartitions.readableAndWritableCnt)
		log.LogInfo(msg)
	}
}

func (c *Cluster) scheduleToLoadDataPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.doLoadDataPartitions()
			}
			time.Sleep(time.Second * 5)
		}
	}()
}

func (c *Cluster) doLoadDataPartitions() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("doLoadDataPartitions occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"doLoadDataPartitions occurred panic")
		}
	}()
	vols := c.allVols()
	for _, vol := range vols {
		vol.loadDataPartition(c)
	}
}

func (c *Cluster) scheduleToCheckReleaseDataPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.releaseDataPartitionAfterLoad()
			}
			time.Sleep(time.Second * defaultIntervalToFreeDataPartition)
		}
	}()
}

// Release the memory used for loading the data partition.
func (c *Cluster) releaseDataPartitionAfterLoad() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("releaseDataPartitionAfterLoad occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"releaseDataPartitionAfterLoad occurred panic")
		}
	}()
	vols := c.copyVols()
	for _, vol := range vols {
		vol.releaseDataPartitions(c.cfg.numberOfDataPartitionsToFree, c.cfg.secondsToFreeDataPartitionAfterLoad)
	}
}

func (c *Cluster) scheduleToCheckHeartbeat() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.checkLeaderAddr()
				c.checkDataNodeHeartbeat()
			}
			time.Sleep(time.Second * defaultIntervalToCheckHeartbeat)
		}
	}()

	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.checkMetaNodeHeartbeat()
			}
			time.Sleep(time.Second * defaultIntervalToCheckHeartbeat)
		}
	}()
}

func (c *Cluster) checkLeaderAddr() {
	leaderID, _ := c.partition.LeaderTerm()
	c.leaderInfo.addr = AddrDatabase[leaderID]
}

func (c *Cluster) checkDataNodeHeartbeat() {
	tasks := make([]*proto.AdminTask, 0)
	c.dataNodes.Range(func(addr, dataNode interface{}) bool {
		node := dataNode.(*DataNode)
		node.checkLiveness()
		task := node.createHeartbeatTask(c.masterAddr())
		tasks = append(tasks, task)
		return true
	})
	c.addDataNodeTasks(tasks)
}

func (c *Cluster) checkMetaNodeHeartbeat() {
	tasks := make([]*proto.AdminTask, 0)
	c.metaNodes.Range(func(addr, metaNode interface{}) bool {
		node := metaNode.(*MetaNode)
		node.checkHeartbeat()
		task := node.createHeartbeatTask(c.masterAddr())
		tasks = append(tasks, task)
		return true
	})
	c.addMetaNodeTasks(tasks)
}

func (c *Cluster) scheduleToCheckMetaPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.checkMetaPartitions()
			}
			time.Sleep(time.Second * time.Duration(c.cfg.IntervalToCheckDataPartition))
		}
	}()
}

func (c *Cluster) checkMetaPartitions() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("checkMetaPartitions occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"checkMetaPartitions occurred panic")
		}
	}()
	vols := c.allVols()
	for _, vol := range vols {
		vol.checkMetaPartitions(c)
	}
}

func (c *Cluster) scheduleToReduceReplicaNum() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsRaftLeader() {
				c.checkVolReduceReplicaNum()
			}
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (c *Cluster) checkVolReduceReplicaNum() {
	defer func() {
		if r := recover(); r != nil {
			log.LogWarnf("checkVolReduceReplicaNum occurred panic,err[%v]", r)
			WarnBySpecialKey(fmt.Sprintf("%v_%v_scheduling_job_panic", c.Name, ModuleName),
				"checkVolReduceReplicaNum occurred panic")
		}
	}()
	vols := c.allVols()
	for _, vol := range vols {
		vol.checkReplicaNum(c)
	}
}

func (c *Cluster) updateMetaNodeBaseInfo(nodeAddr string, id uint64) (err error) {
	c.mnMutex.Lock()
	defer c.mnMutex.Unlock()
	value, ok := c.metaNodes.Load(nodeAddr)
	if !ok {
		err = fmt.Errorf("node %v is not exist", nodeAddr)
	}
	metaNode := value.(*MetaNode)
	if metaNode.ID == id {
		return
	}

	metaNode.ID = id
	if err = c.syncUpdateMetaNode(metaNode); err != nil {
		return
	}
	//partitions := c.getAllMetaPartitionsByMetaNode(nodeAddr)
	return
}

func (c *Cluster) addMetaNode(nodeAddr, zoneName string) (id uint64, err error) {
	c.mnMutex.Lock()
	defer c.mnMutex.Unlock()
	var metaNode *MetaNode
	if value, ok := c.metaNodes.Load(nodeAddr); ok {
		metaNode = value.(*MetaNode)
		return metaNode.ID, nil
	}
	metaNode = newMetaNode(nodeAddr, zoneName, c.Name)
	zone, err := c.t.getZone(zoneName)
	if err != nil {
		zone = c.t.putZoneIfAbsent(newZone(zoneName))
	}
	ns := zone.getAvailNodeSetForMetaNode()
	if ns == nil {
		if ns, err = zone.createNodeSet(c); err != nil {
			goto errHandler
		}
	}
	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		goto errHandler
	}
	metaNode.ID = id
	metaNode.NodeSetID = ns.ID
	if err = c.syncAddMetaNode(metaNode); err != nil {
		goto errHandler
	}
	if err = c.syncUpdateNodeSet(ns); err != nil {
		goto errHandler
	}
	c.t.putMetaNode(metaNode)
	c.metaNodes.Store(nodeAddr, metaNode)
	log.LogInfof("action[addMetaNode],clusterID[%v] metaNodeAddr:%v,nodeSetId[%v],capacity[%v]",
		c.Name, nodeAddr, ns.ID, ns.Capacity)
	return
errHandler:
	err = fmt.Errorf("action[addMetaNode],clusterID[%v] metaNodeAddr:%v err:%v ",
		c.Name, nodeAddr, err.Error())
	log.LogError(errors.Stack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) addDataNode(nodeAddr, zoneName string) (id uint64, err error) {
	c.dnMutex.Lock()
	defer c.dnMutex.Unlock()
	var dataNode *DataNode
	if node, ok := c.dataNodes.Load(nodeAddr); ok {
		dataNode = node.(*DataNode)
		return dataNode.ID, nil
	}

	dataNode = newDataNode(nodeAddr, zoneName, c.Name)
	zone, err := c.t.getZone(zoneName)
	if err != nil {
		zone = c.t.putZoneIfAbsent(newZone(zoneName))
	}
	ns := zone.getAvailNodeSetForDataNode()
	if ns == nil {
		if ns, err = zone.createNodeSet(c); err != nil {
			goto errHandler
		}
	}
	// allocate dataNode id
	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		goto errHandler
	}
	dataNode.ID = id
	dataNode.NodeSetID = ns.ID
	if err = c.syncAddDataNode(dataNode); err != nil {
		goto errHandler
	}
	if err = c.syncUpdateNodeSet(ns); err != nil {
		goto errHandler
	}
	c.t.putDataNode(dataNode)
	c.dataNodes.Store(nodeAddr, dataNode)
	log.LogInfof("action[addDataNode],clusterID[%v] dataNodeAddr:%v,nodeSetId[%v],capacity[%v]",
		c.Name, nodeAddr, ns.ID, ns.Capacity)
	return
errHandler:
	err = fmt.Errorf("action[addDataNode],clusterID[%v] dataNodeAddr:%v err:%v ", c.Name, nodeAddr, err.Error())
	log.LogError(errors.Stack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) checkCorruptDataPartitions() (inactiveDataNodes []string, corruptPartitions []*DataPartition, err error) {
	partitionMap := make(map[uint64]uint8)
	c.dataNodes.Range(func(addr, node interface{}) bool {
		dataNode := node.(*DataNode)
		if !dataNode.isActive {
			inactiveDataNodes = append(inactiveDataNodes, dataNode.Addr)
		}
		return true
	})
	for _, addr := range inactiveDataNodes {
		var dataNode *DataNode
		if dataNode, err = c.dataNode(addr); err != nil {
			return
		}
		for _, partition := range dataNode.PersistenceDataPartitions {
			partitionMap[partition] = partitionMap[partition] + 1
		}
	}

	for partitionID, badNum := range partitionMap {
		var partition *DataPartition
		if partition, err = c.getDataPartitionByID(partitionID); err != nil {
			return
		}
		if badNum > partition.ReplicaNum/2 {
			corruptPartitions = append(corruptPartitions, partition)
		}
	}
	log.LogInfof("clusterID[%v] inactiveDataNodes:%v  corruptPartitions count:[%v]",
		c.Name, inactiveDataNodes, len(corruptPartitions))
	return
}

func (c *Cluster) checkLackReplicaDataPartitions() (lackReplicaDataPartitions []*DataPartition, err error) {
	vols := c.copyVols()
	for _, vol := range vols {
		var dps *DataPartitionMap
		dps = vol.dataPartitions
		for _, dp := range dps.partitions {
			if dp.ReplicaNum > uint8(len(dp.Hosts)) {
				lackReplicaDataPartitions = append(lackReplicaDataPartitions, dp)
			}
		}
	}
	log.LogInfof("clusterID[%v] lackReplicaDataPartitions count:[%v]", c.Name, len(lackReplicaDataPartitions))
	return
}

func (c *Cluster) getDataPartitionByID(partitionID uint64) (dp *DataPartition, err error) {
	vols := c.copyVols()
	for _, vol := range vols {
		if dp, err = vol.getDataPartitionByID(partitionID); err == nil {
			return
		}
	}
	err = dataPartitionNotFound(partitionID)
	return
}

func (c *Cluster) getMetaPartitionByID(id uint64) (mp *MetaPartition, err error) {
	vols := c.copyVols()
	for _, vol := range vols {
		if mp, err = vol.metaPartition(id); err == nil {
			return
		}
	}
	err = metaPartitionNotFound(id)
	return
}

func (c *Cluster) putVol(vol *Vol) {
	c.volMutex.Lock()
	defer c.volMutex.Unlock()
	if _, ok := c.vols[vol.Name]; !ok {
		c.vols[vol.Name] = vol
	}
}

func (c *Cluster) getVol(volName string) (vol *Vol, err error) {
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	vol, ok := c.vols[volName]
	if !ok {
		err = proto.ErrVolNotExists
	}
	return
}

func (c *Cluster) deleteVol(name string) {
	c.volMutex.Lock()
	defer c.volMutex.Unlock()
	delete(c.vols, name)
	return
}

func (c *Cluster) markDeleteVol(name, authKey string) (err error) {
	var (
		vol           *Vol
		serverAuthKey string
	)
	if vol, err = c.getVol(name); err != nil {
		log.LogErrorf("action[markDeleteVol] err[%v]", err)
		return proto.ErrVolNotExists
	}
	serverAuthKey = vol.Owner
	if !matchKey(serverAuthKey, authKey) {
		return proto.ErrVolAuthKeyNotMatch
	}

	vol.Status = markDelete
	if err = c.syncUpdateVol(vol); err != nil {
		vol.Status = normal
		return proto.ErrPersistenceByRaft
	}
	return
}

func (c *Cluster) batchCreateDataPartition(vol *Vol, reqCount int) (err error) {
	var zoneNum int
	for i := 0; i < reqCount; i++ {
		if c.DisableAutoAllocate {
			return
		}
		zoneNum = c.decideZoneNum(vol.crossZone)
		//most of partitions are replicated across 3 zones,but a few partitions are replicated across 2 zones
		if vol.crossZone && i%5 == 0 {
			zoneNum = 2
		}
		if _, err = c.createDataPartition(vol.Name, zoneNum); err != nil {
			log.LogErrorf("action[batchCreateDataPartition] after create [%v] data partition,occurred error,err[%v]", i, err)
			break
		}
	}
	return
}

// Synchronously create a data partition.
// 1. Choose one of the available data nodes.
// 2. Assign it a partition ID.
// 3. Communicate with the data node to synchronously create a data partition.
// - If succeeded, replicate the data through raft and persist it to RocksDB.
// - Otherwise, throw errors
func (c *Cluster) createDataPartition(volName string, zoneNum int) (dp *DataPartition, err error) {
	var (
		vol         *Vol
		partitionID uint64
		targetHosts []string
		targetPeers []proto.Peer
		wg          sync.WaitGroup
	)

	if vol, err = c.getVol(volName); err != nil {
		return
	}
	vol.createDpMutex.Lock()
	defer vol.createDpMutex.Unlock()
	errChannel := make(chan error, vol.dpReplicaNum)
	if targetHosts, targetPeers, err = c.chooseTargetDataNodes("", nil, nil, int(vol.dpReplicaNum), zoneNum, vol.zoneName); err != nil {
		goto errHandler
	}
	if partitionID, err = c.idAlloc.allocateDataPartitionID(); err != nil {
		goto errHandler
	}
	dp = newDataPartition(partitionID, vol.dpReplicaNum, volName, vol.ID)
	dp.Hosts = targetHosts
	dp.Peers = targetPeers
	for _, host := range targetHosts {
		wg.Add(1)
		go func(host string) {
			defer func() {
				wg.Done()
			}()
			var diskPath string
			if diskPath, err = c.syncCreateDataPartitionToDataNode(host, vol.dataPartitionSize, dp, dp.Peers, dp.Hosts, proto.NormalCreateDataPartition); err != nil {
				errChannel <- err
				return
			}
			dp.Lock()
			defer dp.Unlock()
			if err = dp.afterCreation(host, diskPath, c); err != nil {
				errChannel <- err
			}
		}(host)
	}
	wg.Wait()
	select {
	case err = <-errChannel:
		for _, host := range targetHosts {
			wg.Add(1)
			go func(host string) {
				defer func() {
					wg.Done()
				}()
				_, err := dp.getReplica(host)
				if err != nil {
					return
				}
				task := dp.createTaskToDeleteDataPartition(host)
				tasks := make([]*proto.AdminTask, 0)
				tasks = append(tasks, task)
				c.addDataNodeTasks(tasks)
			}(host)
		}
		wg.Wait()
		goto errHandler
	default:
		dp.total = util.DefaultDataPartitionSize
		dp.Status = proto.ReadWrite
	}
	if err = c.syncAddDataPartition(dp); err != nil {
		goto errHandler
	}
	vol.dataPartitions.put(dp)
	log.LogInfof("action[createDataPartition] success,volName[%v],partitionId[%v]", volName, partitionID)
	return
errHandler:
	err = fmt.Errorf("action[createDataPartition],clusterID[%v] vol[%v] Err:%v ", c.Name, volName, err.Error())
	log.LogError(errors.Stack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) syncCreateDataPartitionToDataNode(host string, size uint64, dp *DataPartition, peers []proto.Peer, hosts []string, createType int) (diskPath string, err error) {
	task := dp.createTaskToCreateDataPartition(host, size, peers, hosts, createType)
	dataNode, err := c.dataNode(host)
	if err != nil {
		return
	}
	var resp *proto.Packet
	if resp, err = dataNode.TaskManager.syncSendAdminTask(task); err != nil {
		return
	}
	return string(resp.Data), nil
}

func (c *Cluster) syncCreateMetaPartitionToMetaNode(host string, mp *MetaPartition) (err error) {
	hosts := make([]string, 0)
	hosts = append(hosts, host)
	tasks := mp.buildNewMetaPartitionTasks(hosts, mp.Peers, mp.volName)
	metaNode, err := c.metaNode(host)
	if err != nil {
		return
	}
	if _, err = metaNode.Sender.syncSendAdminTask(tasks[0]); err != nil {
		return
	}
	return
}

//decideZoneNum
//if vol is not cross zone, return 1
//if vol enable cross zone and the zone number of cluster less than defaultReplicaNum return 2
//otherwise, return defaultReplicaNum
func (c *Cluster) decideZoneNum(crossZone bool) (zoneNum int) {
	if !crossZone {
		return 1
	}
	zoneLen := c.t.zoneLen()
	if zoneLen < defaultReplicaNum {
		zoneNum = 2
	} else {
		zoneNum = defaultReplicaNum
	}
	return zoneNum
}
func (c *Cluster) chooseTargetDataNodes(excludeZone string, excludeNodeSets []uint64, excludeHosts []string, replicaNum int, zoneNum int, specifiedZone string) (hosts []string, peers []proto.Peer, err error) {

	var (
		masterZone *Zone
		zones      []*Zone
	)
	excludeZones := make([]string, 0)
	if excludeZone != "" {
		excludeZones = append(excludeZones, excludeZone)
	}
	if replicaNum <= zoneNum {
		zoneNum = replicaNum
	}
	// when creating vol,user specified a zone,we reset zoneNum to 1,to be created partition with specified zone,
	//if specified zone is not writable,we choose a zone randomly
	if specifiedZone != "" {
		zoneNum = 1
		zone, err := c.t.getZone(specifiedZone)
		if err != nil {
			Warn(c.Name, fmt.Sprintf("cluster[%v],specified zone[%v]is not writable", c.Name, specifiedZone))
		} else {
			zones = make([]*Zone, 0)
			zones = append(zones, zone)
		}
	}
	if zones == nil || specifiedZone == "" {
		if zones, err = c.t.allocZonesForDataNode(zoneNum, replicaNum, excludeZones); err != nil {
			return
		}
	}
	//if vol enable cross zone,available zone less than 2,can't create partition
	if zoneNum >= 2 && len(zones) < 2 {
		return nil, nil, fmt.Errorf("no enough zones[%v] to be selected,crossNum[%v]", len(zones), zoneNum)
	}
	if len(zones) == 1 {
		if hosts, peers, err = zones[0].getAvailDataNodeHosts(excludeNodeSets, excludeHosts, replicaNum); err != nil {
			log.LogErrorf("action[chooseTargetDataNodes],err[%v]", err)
			return
		}
		goto result
	}
	hosts = make([]string, 0)
	peers = make([]proto.Peer, 0)
	if excludeHosts == nil {
		excludeHosts = make([]string, 0)
	}
	//replicaNum is equal with the number of allocated zones
	if replicaNum == len(zones) {
		for _, zone := range zones {
			selectedHosts, selectedPeers, e := zone.getAvailDataNodeHosts(excludeNodeSets, excludeHosts, 1)
			if e != nil {
				return nil, nil, errors.NewError(e)
			}
			hosts = append(hosts, selectedHosts...)
			peers = append(peers, selectedPeers...)
		}
		goto result
	}

	// replicaNum larger than the number of allocated zones
	for _, zone := range zones {
		if zone.name != c.lastMasterZoneForDataNode {
			masterZone = zone
			c.lastMasterZoneForDataNode = zone.name
			break
		}
	}
	if masterZone == nil {
		masterZone = zones[0]
	}
	for _, zone := range zones {
		if zone.name == masterZone.name {
			rNum := replicaNum - len(zones) + 1
			selectedHosts, selectedPeers, e := zone.getAvailDataNodeHosts(excludeNodeSets, excludeHosts, rNum)
			if e != nil {
				return nil, nil, errors.NewError(e)
			}
			hosts = append(hosts, selectedHosts...)
			peers = append(peers, selectedPeers...)
		} else {
			selectedHosts, selectedPeers, e := zone.getAvailDataNodeHosts(excludeNodeSets, excludeHosts, 1)
			if e != nil {
				return nil, nil, errors.NewError(e)
			}
			hosts = append(hosts, selectedHosts...)
			peers = append(peers, selectedPeers...)
		}
	}
result:
	log.LogInfof("action[chooseTargetDataNodes] replicaNum[%v],zoneNum[%v],selectedZones[%v],hosts[%v]", replicaNum, zoneNum, len(zones), hosts)
	if len(hosts) != replicaNum {
		log.LogErrorf("action[chooseTargetDataNodes] replicaNum[%v],zoneNum[%v],selectedZones[%v],hosts[%v]", replicaNum, zoneNum, len(zones), hosts)
		return nil, nil, errors.Trace(proto.ErrNoDataNodeToCreateDataPartition, "hosts len[%v],replicaNum[%v],zoneNum[%v],selectedZones[%v]",
			len(hosts), replicaNum, zoneNum, len(zones))
	}
	return
}

func (c *Cluster) dataNode(addr string) (dataNode *DataNode, err error) {
	value, ok := c.dataNodes.Load(addr)
	if !ok {
		err = errors.Trace(dataNodeNotFound(addr), "%v not found", addr)
		return
	}
	dataNode = value.(*DataNode)
	return
}

func (c *Cluster) metaNode(addr string) (metaNode *MetaNode, err error) {
	value, ok := c.metaNodes.Load(addr)
	if !ok {
		err = errors.Trace(metaNodeNotFound(addr), "%v not found", addr)
		return
	}
	metaNode = value.(*MetaNode)
	return
}

func (c *Cluster) getAllDataPartitionByDataNode(addr string) (partitions []*DataPartition) {
	partitions = make([]*DataPartition, 0)
	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, dp := range vol.dataPartitions.partitions {
			for _, host := range dp.Hosts {
				if host == addr {
					partitions = append(partitions, dp)
					break
				}
			}
		}
	}

	return
}

func (c *Cluster) getAllMetaPartitionByMetaNode(addr string) (partitions []*MetaPartition) {
	partitions = make([]*MetaPartition, 0)
	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, mp := range vol.MetaPartitions {
			for _, host := range mp.Hosts {
				if host == addr {
					partitions = append(partitions, mp)
					break
				}
			}
		}
	}

	return
}

func (c *Cluster) getAllDataPartitionIDByDatanode(addr string) (partitionIDs []uint64) {
	partitionIDs = make([]uint64, 0)
	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, dp := range vol.dataPartitions.partitions {
			for _, host := range dp.Hosts {
				if host == addr {
					partitionIDs = append(partitionIDs, dp.PartitionID)
					break
				}
			}
		}
	}

	return
}

func (c *Cluster) getAllMetaPartitionIDByMetaNode(addr string) (partitionIDs []uint64) {
	partitionIDs = make([]uint64, 0)
	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, mp := range vol.MetaPartitions {
			for _, host := range mp.Hosts {
				if host == addr {
					partitionIDs = append(partitionIDs, mp.PartitionID)
					break
				}
			}
		}
	}

	return
}

func (c *Cluster) getAllMetaPartitionsByMetaNode(addr string) (partitions []*MetaPartition) {
	partitions = make([]*MetaPartition, 0)
	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, mp := range vol.MetaPartitions {
			for _, host := range mp.Hosts {
				if host == addr {
					partitions = append(partitions, mp)
					break
				}
			}
		}
	}
	return
}

func (c *Cluster) decommissionDataNode(dataNode *DataNode, destZoneName string, strictFlag bool) (err error) {
	msg := fmt.Sprintf("action[decommissionDataNode], Node[%v],strictMode[%v] OffLine", dataNode.Addr, strictFlag)
	log.LogWarn(msg)
	var wg sync.WaitGroup
	dataNode.ToBeOffline = true
	dataNode.AvailableSpace = 1
	partitions := c.getAllDataPartitionByDataNode(dataNode.Addr)
	errChannel := make(chan error, len(partitions))
	defer func() {
		if err != nil {
			dataNode.ToBeOffline = false
		}
		close(errChannel)
	}()
	for _, dp := range partitions {
		wg.Add(1)
		go func(dp *DataPartition) {
			defer wg.Done()
			if err1 := c.decommissionDataPartition(dataNode.Addr, dp, dataNodeOfflineErr, destZoneName, strictFlag); err1 != nil {
				errChannel <- err1
			}
		}(dp)
	}
	wg.Wait()
	select {
	case err = <-errChannel:
		return
	default:
	}
	if err = c.syncDeleteDataNode(dataNode); err != nil {
		msg = fmt.Sprintf("action[decommissionDataNode],clusterID[%v] Node[%v] OffLine failed,err[%v]",
			c.Name, dataNode.Addr, err)
		Warn(c.Name, msg)
		return
	}
	c.delDataNodeFromCache(dataNode)
	msg = fmt.Sprintf("action[decommissionDataNode],clusterID[%v] Node[%v] OffLine success",
		c.Name, dataNode.Addr)
	Warn(c.Name, msg)
	return
}

func (c *Cluster) delDataNodeFromCache(dataNode *DataNode) {
	c.dataNodes.Delete(dataNode.Addr)
	c.t.deleteDataNode(dataNode)
	go dataNode.clean()
}

// Decommission a data partition.In strict mode, only if the size of the replica is equal,
// or the number of files is equal, the recovery is considered complete. when it is triggered by migrated dataNode,
// the strict mode is true,otherwise is false.
// 1. Check if we can decommission a data partition. In the following cases, we are not allowed to do so:
// - (a) a replica is not in the latest host list;
// - (b) there is already a replica been taken offline;
// - (c) the remaining number of replicas is less than the majority
// 2. Choose a new data node.
// 3. synchronized decommission data partition
// 4. synchronized create a new data partition
// 5. Set the data partition as readOnly.
// 6. persistent the new host list
func (c *Cluster) decommissionDataPartition(offlineAddr string, dp *DataPartition, errMsg, destZoneName string, strictMode bool) (err error) {
	var (
		targetHosts []string
		newAddr     string
		msg         string
		dataNode    *DataNode
		replica     *DataReplica
	)
	dp.RLock()
	if ok := dp.hasHost(offlineAddr); !ok {
		dp.RUnlock()
		return
	}
	replica, _ = dp.getReplica(offlineAddr)
	dp.RUnlock()
	if err = c.validateDecommissionDataPartition(dp, offlineAddr); err != nil {
		goto errHandler
	}

	if dataNode, err = c.dataNode(offlineAddr); err != nil {
		goto errHandler
	}

	if dataNode.ZoneName == "" {
		err = fmt.Errorf("dataNode[%v] zone is nil", dataNode.Addr)
		goto errHandler
	}
	if targetHosts, err = c.chooseTargetHostsForOfflineDataPartition(dataNode, dp, destZoneName); err != nil {
		goto errHandler
	}

	if err = c.removeDataReplica(dp, offlineAddr, false, strictMode); err != nil {
		goto errHandler
	}
	newAddr = targetHosts[0]
	if err = c.addDataReplica(dp, newAddr); err != nil {
		goto errHandler
	}

	dp.Lock()
	dp.Status = proto.ReadOnly
	dp.isRecover = true
	c.syncUpdateDataPartition(dp)
	dp.Unlock()
	if strictMode {
		c.putMigratedDataPartitionIDs(replica, offlineAddr, dp.PartitionID)
	} else {
		c.putBadDataPartitionIDs(replica, offlineAddr, dp.PartitionID)
	}
	log.LogWarnf("clusterID[%v] partitionID:%v  on Node:%v offline success,newHost[%v],PersistenceHosts:[%v]",
		c.Name, dp.PartitionID, offlineAddr, newAddr, dp.Hosts)
	return
errHandler:
	msg = fmt.Sprintf(errMsg + " clusterID[%v] partitionID:%v  on Node:%v  "+
		"Then Fix It on newHost:%v   Err:%v , PersistenceHosts:%v  ",
		c.Name, dp.PartitionID, offlineAddr, newAddr, err, dp.Hosts)
	if err != nil {
		Warn(c.Name, msg)
		err = fmt.Errorf("vol[%v],partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
	}
	return
}

func (c *Cluster) chooseTargetHostsForOfflineDataPartition(dataNode *DataNode, dp *DataPartition, destZoneName string) (targetHosts []string, err error) {
	var (
		zone            *Zone
		ns              *nodeSet
		excludeNodeSets []uint64
		zones           []string
		excludeZone     string
	)

	if destZoneName != "" {
		if zone, err = c.t.getZone(destZoneName); err != nil {
			return
		}
		if targetHosts, _, err = zone.getAvailDataNodeHosts(excludeNodeSets, dp.Hosts, 1); err != nil {
			return
		}
	} else {
		if zone, err = c.t.getZone(dataNode.ZoneName); err != nil {
			return
		}
		if ns, err = zone.getNodeSet(dataNode.NodeSetID); err != nil {
			return
		}
		if targetHosts, _, err = ns.getAvailDataNodeHosts(dp.Hosts, 1); err != nil {
			// select data nodes from the other node set in same zone
			excludeNodeSets = append(excludeNodeSets, ns.ID)
			if targetHosts, _, err = zone.getAvailDataNodeHosts(excludeNodeSets, dp.Hosts, 1); err != nil {
				// select data nodes from the other zone
				zones = dp.getLiveZones(dataNode.Addr)
				if len(zones) == 0 {
					excludeZone = zone.name
				} else {
					excludeZone = zones[0]
				}
				if targetHosts, _, err = c.chooseTargetDataNodes(excludeZone, excludeNodeSets, dp.Hosts, 1, 1, ""); err != nil {
					return
				}
			}
		}
	}
	return
}

func (c *Cluster) validateDecommissionDataPartition(dp *DataPartition, offlineAddr string) (err error) {
	dp.RLock()
	defer dp.RUnlock()
	var vol *Vol
	if vol, err = c.getVol(dp.VolName); err != nil {
		return
	}

	if err = dp.hasMissingOneReplica(offlineAddr, int(vol.dpReplicaNum)); err != nil {
		return
	}

	// if the partition can be offline or not
	if err = dp.canBeOffLine(offlineAddr); err != nil {
		return
	}

	if dp.isRecover && !dp.isLatestReplica(offlineAddr) {
		err = fmt.Errorf("vol[%v],data partition[%v] is recovering,[%v] can't be decommissioned", vol.Name, dp.PartitionID, offlineAddr)
		return
	}
	return
}

func (c *Cluster) addDataReplica(dp *DataPartition, addr string) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[addDataReplica],vol[%v],data partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
		}
	}()
	dataNode, err := c.dataNode(addr)
	if err != nil {
		return
	}
	addPeer := proto.Peer{ID: dataNode.ID, Addr: addr}
	if err = c.addDataPartitionRaftMember(dp, addPeer); err != nil {
		return
	}

	if err = c.createDataReplica(dp, addPeer); err != nil {
		return
	}
	return
}

func (c *Cluster) buildAddDataPartitionRaftMemberTaskAndSyncSendTask(dp *DataPartition, addPeer proto.Peer, leaderAddr string) (resp *proto.Packet, err error) {
	defer func() {
		var resultCode uint8
		if resp != nil {
			resultCode = resp.ResultCode
		}
		if err != nil {
			log.LogErrorf("vol[%v],data partition[%v],resultCode[%v],err[%v]", dp.VolName, dp.PartitionID, resultCode, err)
		} else {
			log.LogWarnf("vol[%v],data partition[%v],resultCode[%v],err[%v]", dp.VolName, dp.PartitionID, resultCode, err)
		}
	}()
	task, err := dp.createTaskToAddRaftMember(addPeer, leaderAddr)
	if err != nil {
		return
	}
	leaderDataNode, err := c.dataNode(leaderAddr)
	if err != nil {
		return
	}
	if resp, err = leaderDataNode.TaskManager.syncSendAdminTask(task); err != nil {
		return
	}
	return
}

func (c *Cluster) addDataPartitionRaftMember(dp *DataPartition, addPeer proto.Peer) (err error) {
	dp.Lock()
	defer dp.Unlock()
	if contains(dp.Hosts, addPeer.Addr) {
		err = fmt.Errorf("vol[%v],data partition[%v] has contains host[%v]", dp.VolName, dp.PartitionID, addPeer.Addr)
		return
	}

	var (
		candidateAddrs []string
		leaderAddr     string
	)
	candidateAddrs = make([]string, 0, len(dp.Hosts))
	leaderAddr = dp.getLeaderAddr()
	if leaderAddr != "" && contains(dp.Hosts, leaderAddr) {
		candidateAddrs = append(candidateAddrs, leaderAddr)
	} else {
		leaderAddr = ""
	}
	for _, host := range dp.Hosts {
		if host == leaderAddr {
			continue
		}
		candidateAddrs = append(candidateAddrs, host)
	}
	//send task to leader addr first,if need to retry,then send to other addr
	for index, host := range candidateAddrs {
		if leaderAddr == "" && len(candidateAddrs) < int(dp.ReplicaNum) {
			time.Sleep(retrySendSyncTaskInternal)
		}
		_, err = c.buildAddDataPartitionRaftMemberTaskAndSyncSendTask(dp, addPeer, host)
		if err == nil {
			break
		}
		if index < len(candidateAddrs)-1 {
			time.Sleep(retrySendSyncTaskInternal)
		}
	}
	if err != nil {
		return
	}
	newHosts := make([]string, 0, len(dp.Hosts)+1)
	newPeers := make([]proto.Peer, 0, len(dp.Peers)+1)
	newHosts = append(dp.Hosts, addPeer.Addr)
	newPeers = append(dp.Peers, addPeer)
	if err = dp.update("addDataPartitionRaftMember", dp.VolName, newPeers, newHosts, c); err != nil {
		return
	}
	return
}

func (c *Cluster) createDataReplica(dp *DataPartition, addPeer proto.Peer) (err error) {
	vol, err := c.getVol(dp.VolName)
	if err != nil {
		return
	}
	dp.RLock()
	hosts := make([]string, len(dp.Hosts))
	copy(hosts, dp.Hosts)
	peers := make([]proto.Peer, len(dp.Peers))
	copy(peers, dp.Peers)
	dp.RUnlock()
	diskPath, err := c.syncCreateDataPartitionToDataNode(addPeer.Addr, vol.dataPartitionSize, dp, peers, hosts, proto.DecommissionedCreateDataPartition)
	if err != nil {
		return
	}
	dp.Lock()
	defer dp.Unlock()
	if err = dp.afterCreation(addPeer.Addr, diskPath, c); err != nil {
		return
	}
	if err = dp.update("createDataReplica", dp.VolName, dp.Peers, dp.Hosts, c); err != nil {
		return
	}
	return
}

func (c *Cluster) removeDataReplica(dp *DataPartition, addr string, validate, migrationMode bool) (err error) {
	defer func() {
		if err != nil {
			log.LogErrorf("action[removeDataReplica],vol[%v],data partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
		}
	}()
	if validate == true {
		if err = c.validateDecommissionDataPartition(dp, addr); err != nil {
			return
		}
	}
	ok := c.isRecovering(dp, addr) && !dp.isLatestReplica(addr)
	if ok {
		err = fmt.Errorf("vol[%v],data partition[%v] can't decommision until it has recovered", dp.VolName, dp.PartitionID)
		return
	}
	dataNode, err := c.dataNode(addr)
	if err != nil {
		return
	}
	removePeer := proto.Peer{ID: dataNode.ID, Addr: addr}

	if err = c.removeDataPartitionRaftMember(dp, removePeer, migrationMode); err != nil {
		return
	}
	if err = c.deleteDataReplica(dp, dataNode, migrationMode); err != nil {
		return
	}
	leaderAddr := dp.getLeaderAddrWithLock()
	if leaderAddr != addr {
		return
	}
	if dataNode, err = c.dataNode(dp.Hosts[0]); err != nil {
		return
	}
	if err = dp.tryToChangeLeader(c, dataNode); err != nil {
		return
	}
	return
}

func (c *Cluster) isRecovering(dp *DataPartition, addr string) (isRecover bool) {
	var key string
	dp.RLock()
	defer dp.RUnlock()
	replica, _ := dp.getReplica(addr)
	if replica != nil {
		key = fmt.Sprintf("%s:%s", addr, replica.DiskPath)
	} else {
		key = fmt.Sprintf("%s:%s", addr, "")
	}
	var badPartitionIDs []uint64
	badPartitions, ok := c.BadDataPartitionIds.Load(key)
	if ok {
		badPartitionIDs = badPartitions.([]uint64)
	}
	for _, id := range badPartitionIDs {
		if id == dp.PartitionID {
			isRecover = true
		}
	}
	return
}

func (c *Cluster) removeDataPartitionRaftMember(dp *DataPartition, removePeer proto.Peer, migrationMode bool) (err error) {
	defer func() {
		if err = c.updateDataPartitionOfflinePeerIDWithLock(dp, 0); err != nil {
			log.LogErrorf("action[removeDataPartitionRaftMember] vol[%v],data partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
		}
	}()
	if err = c.updateDataPartitionOfflinePeerIDWithLock(dp, removePeer.ID); err != nil {
		log.LogErrorf("action[removeDataPartitionRaftMember] vol[%v],data partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
		return
	}
	task, err := dp.createTaskToRemoveRaftMember(removePeer)
	if err != nil {
		return
	}
	task.ReserveResource = migrationMode
	leaderAddr := dp.getLeaderAddr()
	leaderDataNode, err := c.dataNode(leaderAddr)
	if _, err = leaderDataNode.TaskManager.syncSendAdminTask(task); err != nil {
		log.LogErrorf("action[removeDataPartitionRaftMember] vol[%v],data partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
		return
	}
	newHosts := make([]string, 0, len(dp.Hosts)-1)
	for _, host := range dp.Hosts {
		if host == removePeer.Addr {
			continue
		}
		newHosts = append(newHosts, host)
	}
	newPeers := make([]proto.Peer, 0, len(dp.Peers)-1)
	for _, peer := range dp.Peers {
		if peer.ID == removePeer.ID && peer.Addr == removePeer.Addr {
			continue
		}
		newPeers = append(newPeers, peer)
	}
	dp.Lock()
	if err = dp.update("removeDataPartitionRaftMember", dp.VolName, newPeers, newHosts, c); err != nil {
		dp.Unlock()
		return
	}
	dp.Unlock()
	return
}
func (c *Cluster) updateDataPartitionOfflinePeerIDWithLock(dp *DataPartition, peerID uint64) (err error) {
	dp.Lock()
	defer dp.Unlock()
	dp.OfflinePeerID = peerID
	if err = dp.update("updateDataPartitionOfflinePeerIDWithLock", dp.VolName, dp.Peers, dp.Hosts, c); err != nil {
		return
	}
	return
}

func (c *Cluster) deleteDataReplica(dp *DataPartition, dataNode *DataNode, migrationMode bool) (err error) {
	dp.Lock()
	// in case dataNode is unreachable,update meta first.
	dp.removeReplicaByAddr(dataNode.Addr)
	dp.checkAndRemoveMissReplica(dataNode.Addr)
	if err = dp.update("deleteDataReplica", dp.VolName, dp.Peers, dp.Hosts, c); err != nil {
		dp.Unlock()
		return
	}
	task := dp.createTaskToDeleteDataPartition(dataNode.Addr)
	dp.Unlock()
	if migrationMode {
		return
	}
	_, err = dataNode.TaskManager.syncSendAdminTask(task)
	if err != nil {
		log.LogErrorf("action[deleteDataReplica] vol[%v],data partition[%v],err[%v]", dp.VolName, dp.PartitionID, err)
	}
	return nil
}

func (c *Cluster) putBadMetaPartitions(addr string, partitionID uint64) {
	newBadPartitionIDs := make([]uint64, 0)
	badPartitionIDs, ok := c.BadMetaPartitionIds.Load(addr)
	if ok {
		newBadPartitionIDs = badPartitionIDs.([]uint64)
	}
	newBadPartitionIDs = append(newBadPartitionIDs, partitionID)
	c.BadMetaPartitionIds.Store(addr, newBadPartitionIDs)
}

func (c *Cluster) putBadDataPartitionIDs(replica *DataReplica, addr string, partitionID uint64) {
	var key string
	newBadPartitionIDs := make([]uint64, 0)
	if replica != nil {
		key = fmt.Sprintf("%s:%s", addr, replica.DiskPath)
	} else {
		key = fmt.Sprintf("%s:%s", addr, "")
	}
	badPartitionIDs, ok := c.BadDataPartitionIds.Load(key)
	if ok {
		newBadPartitionIDs = badPartitionIDs.([]uint64)
	}
	newBadPartitionIDs = append(newBadPartitionIDs, partitionID)
	c.BadDataPartitionIds.Store(key, newBadPartitionIDs)
}

func (c *Cluster) decommissionMetaNode(metaNode *MetaNode, strictMode bool) (err error) {
	msg := fmt.Sprintf("action[decommissionMetaNode],clusterID[%v] Node[%v] begin", c.Name, metaNode.Addr)
	log.LogWarn(msg)
	var wg sync.WaitGroup
	metaNode.ToBeOffline = true
	metaNode.MaxMemAvailWeight = 1
	partitions := c.getAllMetaPartitionByMetaNode(metaNode.Addr)
	errChannel := make(chan error, len(partitions))
	defer func() {
		metaNode.ToBeOffline = false
		close(errChannel)
	}()
	for _, mp := range partitions {
		wg.Add(1)
		go func(mp *MetaPartition) {
			defer wg.Done()
			if err1 := c.decommissionMetaPartition(metaNode.Addr, mp, strictMode); err1 != nil {
				errChannel <- err1
			}
		}(mp)
	}
	wg.Wait()
	select {
	case err = <-errChannel:
		return
	default:
	}
	if err = c.syncDeleteMetaNode(metaNode); err != nil {
		msg = fmt.Sprintf("action[decommissionMetaNode],clusterID[%v] Node[%v] OffLine failed,err[%v]",
			c.Name, metaNode.Addr, err)
		Warn(c.Name, msg)
		return
	}
	c.deleteMetaNodeFromCache(metaNode)
	msg = fmt.Sprintf("action[decommissionMetaNode],clusterID[%v] Node[%v] OffLine success", c.Name, metaNode.Addr)
	Warn(c.Name, msg)
	return
}

func (c *Cluster) deleteMetaNodeFromCache(metaNode *MetaNode) {
	c.metaNodes.Delete(metaNode.Addr)
	c.t.deleteMetaNode(metaNode)
	go metaNode.clean()
}

func (c *Cluster) updateVol(name, authKey, zoneName, description string, capacity uint64, replicaNum uint8, followerRead, authenticate, enableToken bool) (err error) {
	var (
		vol             *Vol
		serverAuthKey   string
		oldDpReplicaNum uint8
		oldCapacity     uint64
		oldFollowerRead bool
		oldAuthenticate bool
		oldEnableToken  bool
		oldZoneName     string
		oldDescription  string
	)
	if vol, err = c.getVol(name); err != nil {
		log.LogErrorf("action[updateVol] err[%v]", err)
		err = proto.ErrVolNotExists
		goto errHandler
	}
	vol.Lock()
	defer vol.Unlock()
	serverAuthKey = vol.Owner
	if !matchKey(serverAuthKey, authKey) {
		return proto.ErrVolAuthKeyNotMatch
	}
	if capacity < vol.Capacity {
		err = fmt.Errorf("capacity[%v] less than old capacity[%v]", capacity, vol.Capacity)
		goto errHandler
	}
	if replicaNum > vol.dpReplicaNum {
		err = fmt.Errorf("don't support new replicaNum[%v] larger than old dpReplicaNum[%v]", replicaNum, vol.dpReplicaNum)
		goto errHandler
	}
	if enableToken == true && len(vol.tokens) == 0 {
		if err = c.createToken(vol, proto.ReadOnlyToken); err != nil {
			goto errHandler
		}
		if err = c.createToken(vol, proto.ReadWriteToken); err != nil {
			goto errHandler
		}
	}

	oldZoneName = vol.zoneName
	if vol.crossZone && zoneName != "" {
		err = fmt.Errorf("only the vol which don't across zones,can specified zoneName")
		goto errHandler
	}
	if zoneName != "" {
		_, err = c.t.getZone(zoneName)
		if err != nil {
			goto errHandler
		}
	}
	vol.zoneName = zoneName
	oldCapacity = vol.Capacity
	oldDpReplicaNum = vol.dpReplicaNum
	oldFollowerRead = vol.FollowerRead
	oldAuthenticate = vol.authenticate
	oldEnableToken = vol.enableToken
	oldDescription = vol.description
	vol.Capacity = capacity
	vol.FollowerRead = followerRead
	vol.authenticate = authenticate
	vol.enableToken = enableToken
	if description != "" {
		vol.description = description
	}
	//only reduced replica num is supported
	if replicaNum != 0 && replicaNum < vol.dpReplicaNum {
		vol.dpReplicaNum = replicaNum
	}
	if err = c.syncUpdateVol(vol); err != nil {
		vol.Capacity = oldCapacity
		vol.dpReplicaNum = oldDpReplicaNum
		vol.FollowerRead = oldFollowerRead
		vol.authenticate = oldAuthenticate
		vol.enableToken = oldEnableToken
		vol.zoneName = oldZoneName
		vol.description = oldDescription
		log.LogErrorf("action[updateVol] vol[%v] err[%v]", name, err)
		err = proto.ErrPersistenceByRaft
		goto errHandler
	}
	return
errHandler:
	err = fmt.Errorf("action[updateVol], clusterID[%v] name:%v, err:%v ", c.Name, name, err.Error())
	log.LogError(errors.Stack(err))
	Warn(c.Name, err.Error())
	return
}

// Create a new volume.
// By default we create 3 meta partitions and 10 data partitions during initialization.
func (c *Cluster) createVol(name, owner, zoneName, description string, mpCount, dpReplicaNum, size, capacity int, followerRead, authenticate, crossZone, enableToken bool) (vol *Vol, err error) {
	var (
		dataPartitionSize       uint64
		readWriteDataPartitions int
	)
	if size == 0 {
		dataPartitionSize = util.DefaultDataPartitionSize
	} else {
		dataPartitionSize = uint64(size) * util.GB
	}

	if crossZone && c.t.zoneLen() <= 1 {
		return nil, fmt.Errorf("cluster has one zone,can't cross zone")
	}
	if crossZone && zoneName != "" {
		return nil, fmt.Errorf("only the vol which don't across zones,can specified zoneName")
	}
	if zoneName != "" {
		if _, err = c.t.getZone(zoneName); err != nil {
			return
		}
	} else if !crossZone {
		zoneName = DefaultZoneName
	}
	if vol, err = c.doCreateVol(name, owner, zoneName, description, dataPartitionSize, uint64(capacity), dpReplicaNum, followerRead, authenticate, crossZone, enableToken); err != nil {
		goto errHandler
	}
	if err = vol.initMetaPartitions(c, mpCount); err != nil {
		vol.Status = markDelete
		if e := vol.deleteVolFromStore(c); e != nil {
			log.LogErrorf("action[createVol] failed,vol[%v] err[%v]", vol.Name, e)
		}
		c.deleteVol(name)
		err = fmt.Errorf("action[createVol] initMetaPartitions failed,err[%v]", err)
		goto errHandler
	}
	for retryCount := 0; readWriteDataPartitions < defaultInitDataPartitionCnt && retryCount < 3; retryCount++ {
		_ = vol.initDataPartitions(c)
		readWriteDataPartitions = len(vol.dataPartitions.partitionMap)
	}
	vol.dataPartitions.readableAndWritableCnt = readWriteDataPartitions
	vol.updateViewCache(c)
	log.LogInfof("action[createVol] vol[%v],readableAndWritableCnt[%v]", name, readWriteDataPartitions)
	return

errHandler:
	err = fmt.Errorf("action[createVol], clusterID[%v] name:%v, err:%v ", c.Name, name, err)
	log.LogError(errors.Stack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) doCreateVol(name, owner, zoneName, description string, dpSize, capacity uint64, dpReplicaNum int, followerRead, authenticate, crossZone, enableToken bool) (vol *Vol, err error) {
	var id uint64
	c.createVolMutex.Lock()
	defer c.createVolMutex.Unlock()
	var createTime = time.Now().Unix() // record unix seconds of volume create time
	if _, err = c.getVol(name); err == nil {
		err = proto.ErrDuplicateVol
		goto errHandler
	}
	id, err = c.idAlloc.allocateCommonID()
	if err != nil {
		goto errHandler
	}
	vol = newVol(id, name, owner, zoneName, dpSize, capacity, uint8(dpReplicaNum), defaultReplicaNum, followerRead, authenticate, crossZone, enableToken, createTime, description)
	// refresh oss secure
	vol.refreshOSSSecure()
	if err = c.syncAddVol(vol); err != nil {
		goto errHandler
	}
	c.putVol(vol)
	if enableToken {
		if err = c.createToken(vol, proto.ReadOnlyToken); err != nil {
			goto errHandler
		}
		if err = c.createToken(vol, proto.ReadWriteToken); err != nil {
			goto errHandler
		}
	}
	return
errHandler:
	err = fmt.Errorf("action[doCreateVol], clusterID[%v] name:%v, err:%v ", c.Name, name, err.Error())
	log.LogError(errors.Stack(err))
	Warn(c.Name, err.Error())
	return
}

// Update the upper bound of the inode ids in a meta partition.
func (c *Cluster) updateInodeIDRange(volName string, start uint64) (err error) {

	var (
		maxPartitionID uint64
		vol            *Vol
		partition      *MetaPartition
	)

	if vol, err = c.getVol(volName); err != nil {
		log.LogErrorf("action[updateInodeIDRange]  vol [%v] not found", volName)
		return proto.ErrVolNotExists
	}
	maxPartitionID = vol.maxPartitionID()
	if partition, err = vol.metaPartition(maxPartitionID); err != nil {
		log.LogErrorf("action[updateInodeIDRange]  mp[%v] not found", maxPartitionID)
		return proto.ErrMetaPartitionNotExists
	}
	adjustStart := start
	if adjustStart < partition.Start {
		adjustStart = partition.Start
	}
	if adjustStart < partition.MaxInodeID {
		adjustStart = partition.MaxInodeID
	}
	adjustStart = adjustStart + defaultMetaPartitionInodeIDStep
	log.LogWarnf("vol[%v],maxMp[%v],start[%v],adjustStart[%v]", volName, maxPartitionID, start, adjustStart)
	if err = vol.splitMetaPartition(c, partition, adjustStart); err != nil {
		log.LogErrorf("action[updateInodeIDRange]  mp[%v] err[%v]", partition.PartitionID, err)
	}
	return
}

// Choose the target hosts from the available zones and meta nodes.
func (c *Cluster) chooseTargetMetaHosts(excludeZone string, excludeNodeSets []uint64, excludeHosts []string, replicaNum int, crossZone bool, specifiedZone string) (hosts []string, peers []proto.Peer, err error) {
	var (
		zones      []*Zone
		masterZone *Zone
	)
	excludeZones := make([]string, 0)
	if excludeZone != "" {
		excludeZones = append(excludeZones, excludeZone)
	}
	zoneNum := c.decideZoneNum(crossZone)
	if replicaNum < zoneNum {
		zoneNum = replicaNum
	}
	// when creating vol,user specified a zone,we reset zoneNum to 1,to be created partition with specified zone,
	//if specified zone is not writable,we choose a zone randomly
	if specifiedZone != "" {
		zoneNum = 1
		zone, err := c.t.getZone(specifiedZone)
		if err != nil {
			Warn(c.Name, fmt.Sprintf("cluster[%v],specified zone[%v]is not writable", c.Name, specifiedZone))
		} else {
			zones = make([]*Zone, 0)
			zones = append(zones, zone)
		}
	}
	if zones == nil || specifiedZone == "" {
		if zones, err = c.t.allocZonesForMetaNode(zoneNum, replicaNum, excludeZones); err != nil {
			return
		}
	}

	if crossZone && len(zones) < 2 {
		log.LogWarn(fmt.Sprintf("action[chooseTargetMetaNodes] ,no enough zones [%v] to be selected, expect select [%v] zones", len(zones), zoneNum))
		return nil, nil, fmt.Errorf("action[chooseTargetMetaNodes] no enough zones [%v] to be selected, expect select [%v] zones", len(zones), zoneNum)
	}
	if len(zones) == 1 {
		if hosts, peers, err = zones[0].getAvailMetaNodeHosts(excludeNodeSets, excludeHosts, replicaNum); err != nil {
			log.LogErrorf("action[chooseTargetMetaNodes],err[%v]", err)
			return
		}
		return
	}
	hosts = make([]string, 0)
	peers = make([]proto.Peer, 0)
	if excludeHosts == nil {
		excludeHosts = make([]string, 0)
	}
	//replicaNum is equal with the number of allocated zones
	if replicaNum == len(zones) {
		for _, zone := range zones {
			selectedHosts, selectedPeers, e := zone.getAvailMetaNodeHosts(excludeNodeSets, excludeHosts, 1)
			if e != nil {
				return nil, nil, errors.NewError(e)
			}
			hosts = append(hosts, selectedHosts...)
			peers = append(peers, selectedPeers...)
		}
		goto result
	}

	// replicaNum larger than with the number of allocated zones
	for _, zone := range zones {
		if zone.name != c.lastMasterZoneForMetaNode {
			masterZone = zone
			c.lastMasterZoneForMetaNode = zone.name
			break
		}
	}
	if masterZone == nil {
		masterZone = zones[0]
	}
	for _, zone := range zones {
		if zone.name == masterZone.name {
			rNum := replicaNum - len(zones) + 1
			selectedHosts, selectedPeers, e := zone.getAvailMetaNodeHosts(excludeNodeSets, excludeHosts, rNum)
			if e != nil {
				return nil, nil, errors.NewError(e)
			}
			hosts = append(hosts, selectedHosts...)
			peers = append(peers, selectedPeers...)
		} else {
			selectedHosts, selectedPeers, e := zone.getAvailMetaNodeHosts(excludeNodeSets, excludeHosts, 1)
			if e != nil {
				return nil, nil, errors.NewError(e)
			}
			hosts = append(hosts, selectedHosts...)
			peers = append(peers, selectedPeers...)
		}
	}
result:
	log.LogInfof("action[chooseTargetMetaHosts] replicaNum[%v],zoneNum[%v],selectedZones[%v],hosts[%v]", replicaNum, zoneNum, len(zones), hosts)
	if len(hosts) != replicaNum {
		return nil, nil, errors.Trace(proto.ErrNoMetaNodeToCreateMetaPartition, "hosts len[%v],replicaNum[%v]", len(hosts), replicaNum)
	}
	return
}

func (c *Cluster) dataNodeCount() (len int) {
	c.dataNodes.Range(func(key, value interface{}) bool {
		len++
		return true
	})
	return
}

func (c *Cluster) metaNodeCount() (len int) {
	c.metaNodes.Range(func(key, value interface{}) bool {
		len++
		return true
	})
	return
}

func (c *Cluster) allDataNodes() (dataNodes []proto.NodeView) {
	dataNodes = make([]proto.NodeView, 0)
	c.dataNodes.Range(func(addr, node interface{}) bool {
		dataNode := node.(*DataNode)
		dataNodes = append(dataNodes, proto.NodeView{Addr: dataNode.Addr, Status: dataNode.isActive, ID: dataNode.ID, IsWritable: dataNode.isWriteAble()})
		return true
	})
	return
}

func (c *Cluster) allMetaNodes() (metaNodes []proto.NodeView) {
	metaNodes = make([]proto.NodeView, 0)
	c.metaNodes.Range(func(addr, node interface{}) bool {
		metaNode := node.(*MetaNode)
		metaNodes = append(metaNodes, proto.NodeView{ID: metaNode.ID, Addr: metaNode.Addr, Status: metaNode.IsActive, IsWritable: metaNode.isWritable()})
		return true
	})
	return
}

func (c *Cluster) allVolNames() (vols []string) {
	vols = make([]string, 0)
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for name := range c.vols {
		vols = append(vols, name)
	}
	return
}

func (c *Cluster) copyVols() (vols map[string]*Vol) {
	vols = make(map[string]*Vol, 0)
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for name, vol := range c.vols {
		vols[name] = vol
	}
	return
}

// Return all the volumes except the ones that have been marked to be deleted.
func (c *Cluster) allVols() (vols map[string]*Vol) {
	vols = make(map[string]*Vol, 0)
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for name, vol := range c.vols {
		if vol.Status == normal {
			vols[name] = vol
		}
	}
	return
}

func (c *Cluster) getDataPartitionCount() (count int) {
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for _, vol := range c.vols {
		count = count + len(vol.dataPartitions.partitions)
	}
	return
}

func (c *Cluster) getMetaPartitionCount() (count int) {
	vols := c.copyVols()
	for _, vol := range vols {
		count = count + len(vol.MetaPartitions)
	}
	return count
}

func (c *Cluster) setMetaNodeThreshold(threshold float32) (err error) {
	oldThreshold := c.cfg.MetaNodeThreshold
	c.cfg.MetaNodeThreshold = threshold
	if err = c.syncPutCluster(); err != nil {
		log.LogErrorf("action[setMetaNodeThreshold] err[%v]", err)
		c.cfg.MetaNodeThreshold = oldThreshold
		err = proto.ErrPersistenceByRaft
		return
	}
	return
}

func (c *Cluster) setMetaNodeDeleteBatchCount(val uint64) (err error) {
	oldVal := atomic.LoadUint64(&c.cfg.MetaNodeDeleteBatchCount)
	atomic.StoreUint64(&c.cfg.MetaNodeDeleteBatchCount, val)
	if err = c.syncPutCluster(); err != nil {
		log.LogErrorf("action[setMetaNodeDeleteBatchCount] err[%v]", err)
		atomic.StoreUint64(&c.cfg.MetaNodeDeleteBatchCount, oldVal)
		err = proto.ErrPersistenceByRaft
		return
	}
	return
}

func (c *Cluster) setDataNodeDeleteLimitRate(val uint64) (err error) {
	oldVal := atomic.LoadUint64(&c.cfg.DataNodeDeleteLimitRate)
	atomic.StoreUint64(&c.cfg.DataNodeDeleteLimitRate, val)
	if err = c.syncPutCluster(); err != nil {
		log.LogErrorf("action[setDataNodeDeleteLimitRate] err[%v]", err)
		atomic.StoreUint64(&c.cfg.DataNodeDeleteLimitRate, oldVal)
		err = proto.ErrPersistenceByRaft
		return
	}
	return
}

func (c *Cluster) setMetaNodeDeleteWorkerSleepMs(val uint64) (err error) {
	oldVal := atomic.LoadUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs)
	atomic.StoreUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs, val)
	if err = c.syncPutCluster(); err != nil {
		log.LogErrorf("action[setMetaNodeDeleteWorkerSleepMs] err[%v]", err)
		atomic.StoreUint64(&c.cfg.MetaNodeDeleteWorkerSleepMs, oldVal)
		err = proto.ErrPersistenceByRaft
		return
	}
	return
}

func (c *Cluster) setDisableAutoAllocate(disableAutoAllocate bool) (err error) {
	oldFlag := c.DisableAutoAllocate
	c.DisableAutoAllocate = disableAutoAllocate
	if err = c.syncPutCluster(); err != nil {
		log.LogErrorf("action[setDisableAutoAllocate] err[%v]", err)
		c.DisableAutoAllocate = oldFlag
		err = proto.ErrPersistenceByRaft
		return
	}
	return
}

func (c *Cluster) clearVols() {
	c.volMutex.Lock()
	defer c.volMutex.Unlock()
	c.vols = make(map[string]*Vol, 0)
}

func (c *Cluster) clearTopology() {
	c.t.clear()
}

func (c *Cluster) clearDataNodes() {
	c.dataNodes.Range(func(key, value interface{}) bool {
		dataNode := value.(*DataNode)
		c.dataNodes.Delete(key)
		dataNode.clean()
		return true
	})
}

func (c *Cluster) clearMetaNodes() {
	c.metaNodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		c.metaNodes.Delete(key)
		metaNode.clean()
		return true
	})
}

func (c *Cluster) setDataNodeToOfflineState(startID, endID uint64, state bool, zoneName string) {
	c.dataNodes.Range(func(key, value interface{}) bool {
		node, ok := value.(*DataNode)
		if !ok {
			return true
		}
		if node.ID < startID || node.ID > endID {
			return true
		}
		if node.ZoneName != zoneName {
			return true
		}
		node.Lock()
		node.ToBeMigrated = state
		node.Unlock()
		return true
	})
}

func (c *Cluster) setMetaNodeToOfflineState(startID, endID uint64, state bool, zoneName string) {
	c.metaNodes.Range(func(key, value interface{}) bool {
		node, ok := value.(*MetaNode)
		if !ok {
			return true
		}
		if node.ID < startID || node.ID > endID {
			return true
		}
		if node.ZoneName != zoneName {
			return true
		}
		node.Lock()
		node.ToBeMigrated = state
		node.Unlock()
		return true
	})
}
