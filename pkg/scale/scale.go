// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scale

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/go-logr/logr"

	"github.com/banzaicloud/go-cruise-control/pkg/api"
	"github.com/banzaicloud/go-cruise-control/pkg/client"
	"github.com/banzaicloud/go-cruise-control/pkg/types"
	"github.com/banzaicloud/koperator/api/v1beta1"
)

var newCruiseControlScaler = createNewDefaultCruiseControlScaler

func NewCruiseControlScaler(ctx context.Context, serverURL string) (CruiseControlScaler, error) {
	return newCruiseControlScaler(ctx, serverURL)
}

func createNewDefaultCruiseControlScaler(ctx context.Context, serverURL string) (CruiseControlScaler, error) {
	log := logr.FromContextOrDiscard(ctx).WithName("Scaler")

	cfg := &client.Config{
		ServerURL: serverURL,
		UserAgent: "koperator",
	}

	cruisecontrol, err := client.NewClient(ctx, cfg)
	if err != nil {
		log.Error(err, "creating Cruise Control client failed")
		return nil, err
	}
	return &cruiseControlScaler{
		log:    log,
		client: cruisecontrol,
	}, nil
}

type cruiseControlScaler struct {
	CruiseControlScaler

	log    logr.Logger
	client *client.Client
}

// Status returns a CruiseControlStatus describing the internal state of Cruise Control.
func (cc *cruiseControlScaler) Status() CruiseControlStatus {
	req := api.StateRequestWithDefaults()
	req.Verbose = true
	resp, err := cc.client.State(req)
	if err != nil {
		cc.log.Error(err, "failed to get Cruise Control state")
		return CruiseControlStatus{}
	}

	goalsReady := true
	if len(resp.Result.AnalyzerState.GoalReadiness) > 0 {
		for _, goal := range resp.Result.AnalyzerState.GoalReadiness {
			if goal.Status != types.GoalReadinessStatusReady {
				goalsReady = false
				break
			}
		}
	}

	return CruiseControlStatus{
		MonitorReady:       resp.Result.MonitorState.State == types.MonitorStateRunning,
		ExecutorReady:      resp.Result.ExecutorState.State == types.ExecutorStateTypeNoTaskInProgress,
		AnalyzerReady:      resp.Result.AnalyzerState.IsProposalReady && goalsReady,
		ProposalReady:      resp.Result.AnalyzerState.IsProposalReady,
		GoalsReady:         goalsReady,
		MonitoredWindows:   resp.Result.MonitorState.NumMonitoredWindows,
		MonitoringCoverage: resp.Result.MonitorState.MonitoringCoveragePercentage,
	}
}

// IsReady returns true if the Analyzer and Monitor components of Cruise Control are in ready state.
func (cc *cruiseControlScaler) IsReady() bool {
	status := cc.Status()
	cc.log.Info("cruise control readiness",
		"analyzer", status.AnalyzerReady,
		"monitor", status.MonitorReady,
		"executor", status.ExecutorReady,
		"goals ready", status.GoalsReady,
		"monitored windows", status.MonitoredWindows,
		"monitoring coverage percentage", status.MonitoringCoverage)
	return status.IsReady()
}

// IsUp returns true if Cruise Control is online.
func (cc *cruiseControlScaler) IsUp() bool {
	_, err := cc.client.State(api.StateRequestWithDefaults())
	return err == nil
}

// GetUserTasks returns list of Result describing User Tasks from Cruise Control for the provided task IDs.
func (cc *cruiseControlScaler) GetUserTasks(taskIDs ...string) ([]*Result, error) {
	req := &api.UserTasksRequest{
		UserTaskIDs: taskIDs,
		Entries:     100,
	}

	resp, err := cc.client.UserTasks(req)
	if err != nil {
		return nil, err
	}

	results := make([]*Result, len(resp.Result.UserTasks))
	for idx, taskInfo := range resp.Result.UserTasks {
		results[idx] = &Result{
			TaskID:    taskInfo.UserTaskID,
			StartedAt: taskInfo.StartMs.UTC().String(),
			State:     v1beta1.CruiseControlUserTaskState(taskInfo.Status.String()),
		}
	}

	return results, nil
}

// AddBrokers requests Cruise Control to add the list of provided brokers to the Kafka cluster
// by reassigning partition replicas to them.
// Request returns an error if not all brokers are available in Cruise Control.
func (cc *cruiseControlScaler) AddBrokers(brokerIDs ...string) (*Result, error) {
	if len(brokerIDs) == 0 {
		return nil, errors.New("no broker id(s) provided for add brokers request")
	}

	brokersToAdd, err := brokerIDsFromStringSlice(brokerIDs)
	if err != nil {
		cc.log.Error(err, "failed to cast broker IDs from string slice")
		return nil, err
	}

	states := []KafkaBrokerState{KafkaBrokerAlive, KafkaBrokerNew}
	availableBrokers, err := cc.BrokersWithState(states...)
	if err != nil {
		cc.log.Error(err, "failed to retrieve list of available brokers from Cruise Control")
		return nil, err
	}

	availableBrokersMap := stringSliceToMap(availableBrokers)
	unavailableBrokerIDs := make([]string, 0, len(brokerIDs))
	for _, id := range brokerIDs {
		if _, ok := availableBrokersMap[id]; !ok {
			unavailableBrokerIDs = append(unavailableBrokerIDs, id)
		}
	}

	if len(unavailableBrokerIDs) > 0 {
		cc.log.Error(err, "there are offline brokers to be added", "broker(s)", unavailableBrokerIDs)
		return nil, errors.New("not all brokers are available which are meant to be added to the Kafka cluster")
	}

	addBrokerReq := &api.AddBrokerRequest{
		AllowCapacityEstimation: true,
		BrokerIDs:               brokersToAdd,
		DataFrom:                types.ProposalDataSourceValidWindows,
		UseReadyDefaultGoals:    true,
	}
	addBrokerResp, err := cc.client.AddBroker(addBrokerReq)
	if err != nil {
		return &Result{
			TaskID:    addBrokerResp.TaskID,
			StartedAt: addBrokerResp.Date,
			State:     v1beta1.CruiseControlTaskCompletedWithError,
			Err:       fmt.Sprintf("%v", err),
		}, err
	}

	return &Result{
		TaskID:    addBrokerResp.TaskID,
		StartedAt: addBrokerResp.Date,
		State:     v1beta1.CruiseControlTaskActive,
	}, nil
}

// RemoveBrokers requests Cruise Control to move partition replicase off from the provided brokers.
// It does not attempt to remove the provided brokers in case none of them are available in Cruise Control.
func (cc *cruiseControlScaler) RemoveBrokers(brokerIDs ...string) (*Result, error) {
	if len(brokerIDs) == 0 {
		return nil, errors.New("no broker id(s) provided for remove brokers request")
	}

	clusterStateReq := api.KafkaClusterStateRequestWithDefaults()
	clusterStateResp, err := cc.client.KafkaClusterState(clusterStateReq)
	if err != nil {
		return nil, err
	}

	brokersToRemove := make([]int32, 0, len(brokerIDs))
	brokerStates := clusterStateResp.Result.KafkaBrokerState
	for _, brokerID := range brokerIDs {
		replicas, ok := brokerStates.ReplicaCountByBrokerID[brokerID]
		if !ok || replicas <= 0 {
			cc.log.Info("removing broker is skipped as it is either not available or has 0 partition replicas",
				"broker_id", brokerID, "replicas", replicas)
			continue
		}
		var bID int
		bID, err = strconv.Atoi(brokerID)
		if err != nil {
			cc.log.Error(err, "failed to cast broker ID from string to integer", "broker_id", brokerID)
			return nil, err
		}
		brokersToRemove = append(brokersToRemove, int32(bID))
	}

	if len(brokersToRemove) == 0 {
		return &Result{
			State: v1beta1.CruiseControlTaskCompleted,
		}, nil
	}

	rmBrokerReq := &api.RemoveBrokerRequest{
		AllowCapacityEstimation: true,
		BrokerIDs:               brokersToRemove,
		DataFrom:                types.ProposalDataSourceValidWindows,
		UseReadyDefaultGoals:    true,
	}
	rmBrokerResp, err := cc.client.RemoveBroker(rmBrokerReq)
	if err != nil {
		return &Result{
			TaskID:    rmBrokerResp.TaskID,
			StartedAt: rmBrokerResp.Date,
			State:     v1beta1.CruiseControlTaskCompletedWithError,
			Err:       fmt.Sprintf("%v", err),
		}, err
	}

	return &Result{
		TaskID:    rmBrokerResp.TaskID,
		StartedAt: rmBrokerResp.Date,
		State:     v1beta1.CruiseControlTaskActive,
	}, nil
}

// RebalanceDisks performs a disk rebalance via Cruise Control for the provided list of brokers.
func (cc *cruiseControlScaler) RebalanceDisks(brokerIDs ...string) (*Result, error) {
	clusterLoadResp, err := cc.client.KafkaClusterLoad(api.KafkaClusterLoadRequestWithDefaults())
	if err != nil {
		return nil, err
	}

	brokerIDsMap := stringSliceToMap(brokerIDs)

	brokersWithEmptyDisks := make([]int32, 0, len(brokerIDs))
	for _, brokerStat := range clusterLoadResp.Result.Brokers {
		if _, ok := brokerIDsMap[strconv.Itoa(int(brokerStat.Broker))]; !ok {
			continue
		}
		for _, diskState := range brokerStat.DiskState {
			if diskState.NumReplicas <= 0 && !diskState.DiskMB.Dead {
				brokersWithEmptyDisks = append(brokersWithEmptyDisks, brokerStat.Broker)
			}
		}
	}

	if len(brokersWithEmptyDisks) == 0 {
		return &Result{
			State: v1beta1.CruiseControlTaskCompleted,
		}, nil
	}

	rebalanceReq := &api.RebalanceRequest{
		AllowCapacityEstimation:       true,
		DestinationBrokerIDs:          brokersWithEmptyDisks,
		DataFrom:                      types.ProposalDataSourceValidWindows,
		UseReadyDefaultGoals:          true,
		ExcludeRecentlyRemovedBrokers: true,
	}
	rebalanceResp, err := cc.client.Rebalance(rebalanceReq)
	if err != nil {
		return &Result{
			TaskID:    rebalanceResp.TaskID,
			StartedAt: rebalanceResp.Date,
			State:     v1beta1.CruiseControlTaskCompletedWithError,
			Err:       fmt.Sprintf("%v", err),
		}, err
	}

	return &Result{
		TaskID:    rebalanceResp.TaskID,
		StartedAt: rebalanceResp.Date,
		State:     v1beta1.CruiseControlTaskActive,
	}, nil
}

// BrokersWithState returns a list of IDs for Kafka brokers which are available in Cruise Control
// and have one of the expected states.
func (cc *cruiseControlScaler) BrokersWithState(states ...KafkaBrokerState) ([]string, error) {
	resp, err := cc.client.KafkaClusterLoad(api.KafkaClusterLoadRequestWithDefaults())
	if err != nil {
		cc.log.Error(err, "getting Kafka cluster load from Cruise Control returned an error")
		return nil, err
	}

	statesMap := kafkaBrokerStatesToMap(states...)
	brokersIDs := make([]string, 0, len(resp.Result.Brokers))
	for _, broker := range resp.Result.Brokers {
		if _, ok := statesMap[broker.BrokerState]; ok {
			brokerID := strconv.Itoa(int(broker.Broker))
			brokersIDs = append(brokersIDs, brokerID)
			cc.log.Info("Kafka broker is available in Cruise Control",
				"broker id", brokerID, "state", broker.BrokerState)
		}
	}
	return brokersIDs, nil
}

// PartitionReplicasByBroker returns the number of partition replicas for every broker in the Kafka cluster.
func (cc *cruiseControlScaler) PartitionReplicasByBroker() (map[string]int32, error) {
	clusterStateReq := api.KafkaClusterStateRequestWithDefaults()
	clusterStateResp, err := cc.client.KafkaClusterState(clusterStateReq)
	if err != nil {
		return nil, err
	}
	return clusterStateResp.Result.KafkaBrokerState.ReplicaCountByBrokerID, nil
}

// BrokerWithLeastPartitionReplicas returns the ID of the broker which host the least partition replicas.
func (cc *cruiseControlScaler) BrokerWithLeastPartitionReplicas() (string, error) {
	var brokerWithLeastPartitionReplicas string

	brokerPartitions, err := cc.PartitionReplicasByBroker()
	if err != nil {
		cc.log.Error(err, "could not retrieve partition map for brokers")
		return brokerWithLeastPartitionReplicas, err
	}

	replicaCount := int32(math.MaxInt32)
	for brokerID, replica := range brokerPartitions {
		if replicaCount > replica {
			replicaCount = replica
			brokerWithLeastPartitionReplicas = brokerID
		}
	}
	return brokerWithLeastPartitionReplicas, nil
}

// LogDirsByBroker returns the ID of the broker which host the least partition replicas.
func (cc *cruiseControlScaler) LogDirsByBroker() (map[string]map[LogDirState][]string, error) {
	resp, err := cc.client.KafkaClusterState(api.KafkaClusterStateRequestWithDefaults())
	if err != nil {
		cc.log.Error(err, "getting Kafka cluster state from Cruise Control returned an error")
		return nil, err
	}

	newLogDirsByBroker := func() map[LogDirState][]string {
		return map[LogDirState][]string{
			LogDirStateOnline:  {},
			LogDirStateOffline: {},
		}
	}

	logDirsByBrokers := make(map[string]map[LogDirState][]string)
	for broker, onlineLogDirs := range resp.Result.KafkaBrokerState.OnlineLogDirsByBrokerID {
		logDirsByBroker, ok := logDirsByBrokers[broker]
		if !ok || logDirsByBroker == nil {
			logDirsByBroker = newLogDirsByBroker()
		}
		logDirsByBroker[LogDirStateOnline] = onlineLogDirs
		logDirsByBrokers[broker] = logDirsByBroker
	}
	for broker, offlineLogDirs := range resp.Result.KafkaBrokerState.OfflineLogDirsByBrokerID {
		logDirsByBroker, ok := logDirsByBrokers[broker]
		if !ok || logDirsByBroker == nil {
			logDirsByBroker = newLogDirsByBroker()
		}
		logDirsByBroker[LogDirStateOffline] = offlineLogDirs
		logDirsByBrokers[broker] = logDirsByBroker
	}
	return logDirsByBrokers, nil
}
