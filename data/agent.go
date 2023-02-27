package data

import (
	"bytes"
	"context"
	"crypto/sha256"
	"gopkg.in/yaml.v3"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
)

// Agent represents a connected Agent.
type Agent struct {
	// Some fields in this struct are exported so that we can render them in the UI.

	// Agent's instance id. This is an immutable field.
	InstanceId InstanceId

	// Connection to the Agent.
	conn types.Connection
	// Mutex to protect Send() operation.
	connMutex sync.Mutex

	// mutex for the fields that follow it.
	mux sync.RWMutex

	// Agent's current status.
	Status *protobufs.AgentToServer

	// The time when the agent has started. Valid only if Status.Health.Up==true
	StartedAt time.Time

	// Effective config reported by the Agent.
	EffectiveConfig map[string]*protobufs.AgentConfigFile

	// Optional special remote config for this particular instance defined by
	// the user in the UI.
	CustomInstanceConfig map[string]string

	// Remote config that we will give to this Agent.
	remoteConfig *protobufs.AgentRemoteConfig

	// Channels to notify when this Agent's status is updated next time.
	statusUpdateWatchers []chan<- struct{}
}

func NewAgent(
	instanceId InstanceId,
	conn types.Connection,
) *Agent {
	return &Agent{
		InstanceId:           instanceId,
		conn:                 conn,
		CustomInstanceConfig: map[string]string{},
		EffectiveConfig:      map[string]*protobufs.AgentConfigFile{},
	}
}

// CloneReadonly returns a copy of the Agent that is safe to read.
// Functions that modify the Agent should not be called on the cloned copy.
func (agent *Agent) CloneReadonly() *Agent {
	agent.mux.RLock()
	defer agent.mux.RUnlock()
	return &Agent{
		InstanceId:           agent.InstanceId,
		Status:               proto.Clone(agent.Status).(*protobufs.AgentToServer),
		EffectiveConfig:      agent.EffectiveConfig,
		CustomInstanceConfig: agent.CustomInstanceConfig,
		remoteConfig:         proto.Clone(agent.remoteConfig).(*protobufs.AgentRemoteConfig),
		StartedAt:            agent.StartedAt,
	}
}

// UpdateStatus updates the status of the Agent struct based on the newly received
// status report and sets appropriate fields in the response message to be sent
// to the Agent.
func (agent *Agent) UpdateStatus(
	statusMsg *protobufs.AgentToServer,
	response *protobufs.ServerToAgent,
) {
	agent.mux.Lock()

	agent.processStatusUpdate(statusMsg, response)

	statusUpdateWatchers := agent.statusUpdateWatchers
	agent.statusUpdateWatchers = nil

	agent.mux.Unlock()

	// Notify watcher outside mutex to avoid blocking the mutex for too long.
	notifyStatusWatchers(statusUpdateWatchers)
}

func notifyStatusWatchers(statusUpdateWatchers []chan<- struct{}) {
	// Notify everyone who is waiting on this Agent's status updates.
	for _, ch := range statusUpdateWatchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (agent *Agent) updateAgentDescription(newStatus *protobufs.AgentToServer) (agentDescrChanged bool) {
	prevStatus := agent.Status

	if agent.Status == nil {
		// First time this Agent reports a status, remember it.
		agent.Status = newStatus
		agentDescrChanged = true
	} else {
		// Not a new Agent. Update the Status.
		agent.Status.SequenceNum = newStatus.SequenceNum

		// Check what's changed in the AgentDescription.
		if newStatus.AgentDescription != nil {
			// If the AgentDescription field is set it means the Agent tells us
			// something is changed in the field since the last status report
			// (or this is the first report).
			// Make full comparison of previous and new descriptions to see if it
			// really is different.
			if prevStatus != nil && proto.Equal(prevStatus.AgentDescription, newStatus.AgentDescription) {
				// Agent description didn't change.
				agentDescrChanged = false
			} else {
				// Yes, the description is different, update it.
				agent.Status.AgentDescription = newStatus.AgentDescription
				agentDescrChanged = true
			}
		} else {
			// AgentDescription field is not set, which means description didn't change.
			agentDescrChanged = false
		}

		// Update remote config status if it is included and is different from what we have.
		if newStatus.RemoteConfigStatus != nil &&
			!proto.Equal(agent.Status.RemoteConfigStatus, newStatus.RemoteConfigStatus) {
			agent.Status.RemoteConfigStatus = newStatus.RemoteConfigStatus
		}
	}
	return agentDescrChanged
}

func (agent *Agent) updateHealth(newStatus *protobufs.AgentToServer) {
	if newStatus.Health == nil {
		return
	}

	agent.Status.Health = newStatus.Health

	if agent.Status != nil && agent.Status.Health != nil && agent.Status.Health.Healthy {
		agent.StartedAt = time.Unix(0, int64(agent.Status.Health.StartTimeUnixNano)).UTC()
	}
}

func (agent *Agent) updateRemoteConfigStatus(newStatus *protobufs.AgentToServer) {
	// Update remote config status if it is included and is different from what we have.
	if newStatus.RemoteConfigStatus != nil {
		agent.Status.RemoteConfigStatus = newStatus.RemoteConfigStatus
	}
}

func (agent *Agent) updateStatusField(newStatus *protobufs.AgentToServer) (agentDescrChanged bool) {
	if agent.Status == nil {
		// First time this Agent reports a status, remember it.
		agent.Status = newStatus
		agentDescrChanged = true
	}

	agentDescrChanged = agent.updateAgentDescription(newStatus) || agentDescrChanged
	agent.updateRemoteConfigStatus(newStatus)
	agent.updateHealth(newStatus)

	return agentDescrChanged
}

func (agent *Agent) updateEffectiveConfig(
	newStatus *protobufs.AgentToServer,
	response *protobufs.ServerToAgent,
) {
	// Update effective config if provided.
	if newStatus.EffectiveConfig != nil {
		if newStatus.EffectiveConfig.ConfigMap.GetConfigMap() != nil {
			agent.Status.EffectiveConfig = newStatus.EffectiveConfig
			agent.EffectiveConfig = newStatus.EffectiveConfig.ConfigMap.GetConfigMap()
			//for key, file := range agent.EffectiveConfig {
			//	agent.CustomInstanceConfig[key] = file.String()
			//}
			agent.calcRemoteConfig()
		}
	}
}

func (agent *Agent) hasCapability(capability protobufs.AgentCapabilities) bool {
	return agent.Status.Capabilities&uint64(capability) != 0
}

func (agent *Agent) processStatusUpdate(
	newStatus *protobufs.AgentToServer,
	response *protobufs.ServerToAgent,
) {
	// We don't have any status for this Agent, or we lost the previous status update from the Agent, so our
	// current status is not up-to-date.
	lostPreviousUpdate := (agent.Status == nil) || (agent.Status != nil && agent.Status.SequenceNum+1 != newStatus.SequenceNum)

	agentDescrChanged := agent.updateStatusField(newStatus)

	// Check if any fields were omitted in the status report.
	effectiveConfigOmitted := newStatus.EffectiveConfig == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig)

	packageStatusesOmitted := newStatus.PackageStatuses == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsPackageStatuses)

	remoteConfigStatusOmitted := newStatus.RemoteConfigStatus == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig)

	healthOmitted := newStatus.Health == nil &&
		agent.hasCapability(protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth)

	// True if the status was not fully reported.
	statusIsCompressed := effectiveConfigOmitted || packageStatusesOmitted || remoteConfigStatusOmitted || healthOmitted

	if statusIsCompressed && lostPreviousUpdate {
		// The status message is not fully set in the message that we received, but we lost the previous
		// status update. Request full status update from the agent.
		response.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	}

	configChanged := false
	if agentDescrChanged {
		// Agent description is changed.

		// We need to recalculate the config.
		configChanged = agent.calcRemoteConfig()

		// And set connection settings that are appropriate for the Agent description.
		agent.calcConnectionSettings(response)
	}

	// If remote config is changed and different from what the Agent has then
	// send the new remote config to the Agent.
	if configChanged ||
		(agent.Status.RemoteConfigStatus != nil &&
			bytes.Compare(agent.Status.RemoteConfigStatus.LastRemoteConfigHash, agent.remoteConfig.ConfigHash) != 0) {
		// The new status resulted in a change in the config of the Agent or the Agent
		// does not have this config (hash is different). Send the new config the Agent.
		response.RemoteConfig = agent.remoteConfig
	}

	agent.updateEffectiveConfig(newStatus, response)
}

func updateSpecFromConfig(old *protobufs.AgentConfigFile, newSpec []byte) (*protobufs.AgentConfigFile, error) {
	m := make(map[string]interface{})
	err := yaml.Unmarshal(old.GetBody(), &m)
	if err != nil {
		return nil, err
	}
	m2 := make(map[string]interface{})
	err = yaml.Unmarshal(newSpec, &m2)
	if err != nil {
		return nil, err
	}
	m["spec"] = m2
	b, err := yaml.Marshal(m)
	if err != nil {
		return old, err
	}
	if old == nil {
		old = &protobufs.AgentConfigFile{
			Body:        b,
			ContentType: "yaml",
		}
	}
	old.Body = b
	return old, nil
}

func (agent *Agent) DeleteCollector(key string) {
	agent.mux.Lock()
	defer agent.mux.Unlock()
	delete(agent.CustomInstanceConfig, key)
	delete(agent.EffectiveConfig, key)
	configChanged := agent.calcRemoteConfig()
	if !configChanged {
		return
	}
	msg := &protobufs.ServerToAgent{
		RemoteConfig: agent.remoteConfig,
	}
	agent.SendToAgent(msg)
}

// SetCustomConfig sets a custom config for this Agent.
// notifyWhenConfigIsApplied channel is notified after the remote config is applied
// to the Agent and after the Agent reports back the effective config.
// If the provided config is equal to the current remoteConfig of the Agent
// then we will not send any config to the Agent and notifyWhenConfigIsApplied channel
// will be notified immediately. This requires that notifyWhenConfigIsApplied channel
// has a buffer size of at least 1.
func (agent *Agent) SetCustomConfig(
	config *protobufs.AgentConfigMap,
	notifyWhenConfigIsApplied chan<- struct{},
) {
	agent.mux.Lock()

	for key, file := range config.GetConfigMap() {
		agent.CustomInstanceConfig[key] = string(file.Body)
		updated, err := updateSpecFromConfig(agent.EffectiveConfig[key], file.Body)
		if err != nil {
			continue
		}
		agent.EffectiveConfig[key] = updated
	}

	configChanged := agent.calcRemoteConfig()
	if configChanged {
		if notifyWhenConfigIsApplied != nil {
			// The caller wants to be notified when the Agent reports a status
			// update next time. This is typically used in the UI to wait until
			// the configuration changes are propagated successfully to the Agent.
			agent.statusUpdateWatchers = append(
				agent.statusUpdateWatchers,
				notifyWhenConfigIsApplied,
			)
		}
		msg := &protobufs.ServerToAgent{
			RemoteConfig: agent.remoteConfig,
		}
		agent.SendToAgent(msg)
	} else {
		if notifyWhenConfigIsApplied != nil {
			// No config change. We are not going to send config to the Agent and
			// as a result we do not expect status update from the Agent, so we will
			// just notify the waiter that the config change is done.
			notifyWhenConfigIsApplied <- struct{}{}
		}
	}
	agent.mux.Unlock()
}

// calcRemoteConfig calculates the remote config for this Agent. It returns true if
// the calculated new config is different from the existing config stored in
// Agent.remoteConfig.
func (agent *Agent) calcRemoteConfig() bool {
	hash := sha256.New()

	cfg := protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigFile{},
		},
	}

	// Add the custom config for this particular Agent instance. Use empty
	// string as the config file name.
	for key, body := range agent.CustomInstanceConfig {
		cfg.Config.ConfigMap[key] = &protobufs.AgentConfigFile{
			Body: []byte(body),
		}
	}

	// Calculate the hash.
	for k, v := range cfg.Config.ConfigMap {
		hash.Write([]byte(k))
		hash.Write(v.Body)
		hash.Write([]byte(v.ContentType))
	}

	cfg.ConfigHash = hash.Sum(nil)

	configChanged := !isEqualRemoteConfig(agent.remoteConfig, &cfg)

	agent.remoteConfig = &cfg

	return configChanged
}

func isEqualRemoteConfig(c1, c2 *protobufs.AgentRemoteConfig) bool {
	if c1 == c2 {
		return true
	}
	if c1 == nil || c2 == nil {
		return false
	}
	return isEqualConfigSet(c1.Config, c2.Config)
}

func isEqualConfigSet(c1, c2 *protobufs.AgentConfigMap) bool {
	if c1 == c2 {
		return true
	}
	if c1 == nil || c2 == nil {
		return false
	}
	if len(c1.ConfigMap) != len(c2.ConfigMap) {
		return false
	}
	for k, v1 := range c1.ConfigMap {
		v2, ok := c2.ConfigMap[k]
		if !ok {
			return false
		}
		if !isEqualConfigFile(v1, v2) {
			return false
		}
	}
	return true
}

func isEqualConfigFile(f1, f2 *protobufs.AgentConfigFile) bool {
	if f1 == f2 {
		return true
	}
	if f1 == nil || f2 == nil {
		return false
	}
	return bytes.Compare(f1.Body, f2.Body) == 0 && f1.ContentType == f2.ContentType
}

func (agent *Agent) calcConnectionSettings(response *protobufs.ServerToAgent) {
	// Here we can use Agent's description to send the appropriate connection
	// settings to the Agent.
	// In this simple example the connection settings do not depend on the
	// Agent description, so we jst set them directly.

	response.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
		Hash:             nil, // TODO: calc has from settings.
		Opamp:            nil,
		OwnTraces:        nil,
		OwnLogs:          nil,
		OtherConnections: nil,
	}
}

func (agent *Agent) SendToAgent(msg *protobufs.ServerToAgent) {
	agent.connMutex.Lock()
	defer agent.connMutex.Unlock()

	agent.conn.Send(context.Background(), msg)
}
