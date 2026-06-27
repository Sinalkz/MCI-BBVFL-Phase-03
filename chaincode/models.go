package main

// objectType discriminators used in composite keys and in the stored JSON.
const (
	objectTypeParticipant = "participant"
	objectTypeJob         = "job"
	objectTypeEpoch       = "epoch"
)

// Valid participant roles. Exactly these three are accepted by RegisterParticipant.
const (
	RoleInitiator   = "INITIATOR"
	RoleLabelOwner  = "LABEL_OWNER"
	RoleContributor = "CONTRIBUTOR"
)

// TrainingJob lifecycle states.
const (
	StatusOpen      = "OPEN"
	StatusRunning   = "RUNNING"
	StatusFinalized = "FINALIZED"
)

// PrivateCollection is the single private data collection that holds the real
// loss values. Membership is the job's participant orgs (see collections_config.json).
const PrivateCollection = "collectionLossDetails"

// Event names emitted by the state-changing functions.
const (
	EventParticipantRegistered = "ParticipantRegistered"
	EventTrainingJobStarted    = "TrainingJobStarted"
	EventEpochLogged           = "EpochLogged"
	EventTrainingFinalized     = "TrainingFinalized"
	EventRewardsDistributed    = "RewardsDistributed"
)

// Participant is a registered org in the consortium. Stored on the public world
// state, keyed by CreateCompositeKey("participant", []string{mspID}).
type Participant struct {
	ObjectType    string `json:"objectType"`
	MSPID         string `json:"mspID"`
	Role          string `json:"role"`
	DisplayName   string `json:"displayName"`
	RegisteredAt  string `json:"registeredAt"` // RFC3339, derived from tx timestamp
	RewardBalance int    `json:"rewardBalance"`
}

// TrainingJob is a VFL training run. Multi-party-ready: participants is a list,
// not two fixed slots. Exactly one participant is the LABEL_OWNER.
// Keyed by CreateCompositeKey("job", []string{jobId}).
type TrainingJob struct {
	ObjectType     string   `json:"objectType"`
	JobID          string   `json:"jobId"`
	Status         string   `json:"status"`
	InitiatorMSP   string   `json:"initiatorMSP"`
	Participants   []string `json:"participants"`
	LabelOwnerMSP  string   `json:"labelOwnerMSP"`
	TotalEpochs    int      `json:"totalEpochs"`
	FinalModelHash string   `json:"finalModelHash"`
	CreatedAt      string   `json:"createdAt"`
	FinalizedAt    string   `json:"finalizedAt"`
	Rewarded       bool     `json:"rewarded"` // idempotency guard for DistributeRewards
}

// EpochRecord is the PUBLIC audit anchor for one epoch. It carries only the
// hash of the loss, never the loss itself.
// Keyed by CreateCompositeKey("epoch", []string{jobId, fmt.Sprintf("%012d", epoch)}).
type EpochRecord struct {
	ObjectType string `json:"objectType"`
	JobID      string `json:"jobId"`
	Epoch      int    `json:"epoch"`
	LossHash   string `json:"lossHash"` // SHA-256 hex of the canonical PrivateLossDetail JSON
	RecordedAt string `json:"recordedAt"`
}

// PrivateLossDetail is the PRIVATE per-epoch payload. It lives only in
// PrivateCollection and is visible only to collection members. Its canonical
// JSON encoding is what gets hashed into EpochRecord.LossHash.
type PrivateLossDetail struct {
	JobID string  `json:"jobId"`
	Epoch int     `json:"epoch"`
	Loss  float64 `json:"loss"`
}
