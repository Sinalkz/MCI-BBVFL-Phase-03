package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// SmartContract is the VFL audit / identity / reward chaincode. It embeds
// contractapi.Contract so every exported method becomes a transaction function.
type SmartContract struct {
	contractapi.Contract
}

// ---------------------------------------------------------------------------
// Small internal helpers (identity, timestamp, state I/O)
// ---------------------------------------------------------------------------

// callerMSPID returns the MSP ID of the invoking client identity.
func (s *SmartContract) callerMSPID(ctx contractapi.TransactionContextInterface) (string, error) {
	mspID, err := ctx.GetClientIdentity().GetMSPID()
	if err != nil {
		return "", fmt.Errorf("failed to read caller MSPID: %w", err)
	}
	return mspID, nil
}

// txTimestampRFC3339 derives a deterministic RFC3339 timestamp from the
// transaction timestamp. We never use time.Now(): it is non-deterministic and
// would break endorsement (peers would compute different values).
func txTimestampRFC3339(ctx contractapi.TransactionContextInterface) (string, error) {
	ts, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return "", fmt.Errorf("failed to read tx timestamp: %w", err)
	}
	return ts.AsTime().UTC().Format(time.RFC3339), nil
}

// validRole reports whether role is one of the three accepted roles.
func validRole(role string) bool {
	return role == RoleInitiator || role == RoleLabelOwner || role == RoleContributor
}

// getParticipant loads a Participant, returning (nil, nil) when absent.
func (s *SmartContract) getParticipant(ctx contractapi.TransactionContextInterface, mspID string) (*Participant, error) {
	key, err := ctx.GetStub().CreateCompositeKey(objectTypeParticipant, []string{mspID})
	if err != nil {
		return nil, err
	}
	data, err := ctx.GetStub().GetState(key)
	if err != nil {
		return nil, fmt.Errorf("failed to read participant %s: %w", mspID, err)
	}
	if data == nil {
		return nil, nil
	}
	var p Participant
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("failed to decode participant %s: %w", mspID, err)
	}
	return &p, nil
}

// getJob loads a TrainingJob, returning (nil, nil) when absent.
func (s *SmartContract) getJob(ctx contractapi.TransactionContextInterface, jobID string) (*TrainingJob, error) {
	key, err := ctx.GetStub().CreateCompositeKey(objectTypeJob, []string{jobID})
	if err != nil {
		return nil, err
	}
	data, err := ctx.GetStub().GetState(key)
	if err != nil {
		return nil, fmt.Errorf("failed to read job %s: %w", jobID, err)
	}
	if data == nil {
		return nil, nil
	}
	var j TrainingJob
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("failed to decode job %s: %w", jobID, err)
	}
	return &j, nil
}

// putJob persists a TrainingJob to world state under its composite key.
func (s *SmartContract) putJob(ctx contractapi.TransactionContextInterface, job *TrainingJob) error {
	key, err := ctx.GetStub().CreateCompositeKey(objectTypeJob, []string{job.JobID})
	if err != nil {
		return err
	}
	bytes, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return ctx.GetStub().PutState(key, bytes)
}

// ---------------------------------------------------------------------------
// Access-control helpers (used by all five state-changing functions)
// ---------------------------------------------------------------------------

// requireRole verifies the caller is a registered Participant whose role equals
// expectedRole. It returns the caller's Participant record for reuse.
func (s *SmartContract) requireRole(ctx contractapi.TransactionContextInterface, expectedRole string) (*Participant, error) {
	caller, err := s.callerMSPID(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.getParticipant(ctx, caller)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("caller %s is not a registered participant", caller)
	}
	if p.Role != expectedRole {
		return nil, fmt.Errorf("caller %s has role %s but %s is required", caller, p.Role, expectedRole)
	}
	return p, nil
}

// requireJobRole verifies the caller occupies a specific role-bearing field on a
// job. field is either RoleInitiator (checks job.InitiatorMSP) or RoleLabelOwner
// (checks job.LabelOwnerMSP). Returns the caller MSPID for reuse.
func (s *SmartContract) requireJobRole(ctx contractapi.TransactionContextInterface, job *TrainingJob, field string) (string, error) {
	caller, err := s.callerMSPID(ctx)
	if err != nil {
		return "", err
	}
	switch field {
	case RoleInitiator:
		if caller != job.InitiatorMSP {
			return "", fmt.Errorf("caller %s is not the INITIATOR for job %s", caller, job.JobID)
		}
	case RoleLabelOwner:
		if caller != job.LabelOwnerMSP {
			return "", fmt.Errorf("caller %s is not the LABEL_OWNER for job %s", caller, job.JobID)
		}
	default:
		return "", fmt.Errorf("unknown job role field %q", field)
	}
	return caller, nil
}

// ---------------------------------------------------------------------------
// 1. RegisterParticipant
// ---------------------------------------------------------------------------

// RegisterParticipant records the calling org as a Participant. Self-registration
// only: the caller's MSPID must equal mspID. role must be one of the three valid
// roles. Duplicates are rejected. rewardBalance starts at 0.
func (s *SmartContract) RegisterParticipant(ctx contractapi.TransactionContextInterface, mspID, role, displayName string) (*Participant, error) {
	caller, err := s.callerMSPID(ctx)
	if err != nil {
		return nil, err
	}
	if caller != mspID {
		return nil, fmt.Errorf("caller %s may only self-register; cannot register %s", caller, mspID)
	}
	if !validRole(role) {
		return nil, fmt.Errorf("invalid role %q; must be one of %s, %s, %s", role, RoleInitiator, RoleLabelOwner, RoleContributor)
	}

	existing, err := s.getParticipant(ctx, mspID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("participant %s already registered", mspID)
	}

	registeredAt, err := txTimestampRFC3339(ctx)
	if err != nil {
		return nil, err
	}

	p := &Participant{
		ObjectType:    objectTypeParticipant,
		MSPID:         mspID,
		Role:          role,
		DisplayName:   displayName,
		RegisteredAt:  registeredAt,
		RewardBalance: 0,
	}

	key, err := ctx.GetStub().CreateCompositeKey(objectTypeParticipant, []string{mspID})
	if err != nil {
		return nil, err
	}
	bytes, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().PutState(key, bytes); err != nil {
		return nil, err
	}
	if err := ctx.GetStub().SetEvent(EventParticipantRegistered, bytes); err != nil {
		return nil, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 2. StartTrainingJob
// ---------------------------------------------------------------------------

// StartTrainingJob creates a new OPEN job. Only an INITIATOR may call.
// participantsCSV is a comma-separated list of registered participant MSPIDs;
// it must contain >= 2 members, include the initiator, include labelOwnerMSP,
// and labelOwnerMSP's Participant role must be LABEL_OWNER.
func (s *SmartContract) StartTrainingJob(ctx contractapi.TransactionContextInterface, jobID, participantsCSV, labelOwnerMSP string) (*TrainingJob, error) {
	initiator, err := s.requireRole(ctx, RoleInitiator)
	if err != nil {
		return nil, err
	}

	participants := parseCSV(participantsCSV)
	if len(participants) < 2 {
		return nil, fmt.Errorf("a job needs at least 2 participants, got %d", len(participants))
	}

	// All listed participants must be registered.
	for _, mspID := range participants {
		p, err := s.getParticipant(ctx, mspID)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("participant %s is not registered", mspID)
		}
	}

	// The initiator must be a member of the job.
	if !contains(participants, initiator.MSPID) {
		return nil, fmt.Errorf("initiator %s must be among the job participants", initiator.MSPID)
	}

	// labelOwnerMSP must be in the list and actually hold the LABEL_OWNER role.
	if !contains(participants, labelOwnerMSP) {
		return nil, fmt.Errorf("labelOwnerMSP %s must be among the job participants", labelOwnerMSP)
	}
	labelOwner, err := s.getParticipant(ctx, labelOwnerMSP)
	if err != nil {
		return nil, err
	}
	if labelOwner == nil || labelOwner.Role != RoleLabelOwner {
		return nil, fmt.Errorf("participant %s is not a LABEL_OWNER", labelOwnerMSP)
	}

	existing, err := s.getJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("job %s already exists", jobID)
	}

	createdAt, err := txTimestampRFC3339(ctx)
	if err != nil {
		return nil, err
	}

	job := &TrainingJob{
		ObjectType:    objectTypeJob,
		JobID:         jobID,
		Status:        StatusOpen,
		InitiatorMSP:  initiator.MSPID,
		Participants:  participants,
		LabelOwnerMSP: labelOwnerMSP,
		TotalEpochs:   0,
		CreatedAt:     createdAt,
		Rewarded:      false,
	}

	if err := s.putJob(ctx, job); err != nil {
		return nil, err
	}
	eventBytes, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().SetEvent(EventTrainingJobStarted, eventBytes); err != nil {
		return nil, err
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// 3. LogEpochResult (loss arrives via transient data, never as a plain arg)
// ---------------------------------------------------------------------------

// LogEpochResult records one training epoch. Only the job's LABEL_OWNER may call.
// The loss is read from the transient map under key "loss" (a JSON number), is
// written to the private collection, and its SHA-256 hash is written to the
// public EpochRecord. The raw loss never touches the public ledger or events.
func (s *SmartContract) LogEpochResult(ctx contractapi.TransactionContextInterface, jobID string, epoch int) (*EpochRecord, error) {
	job, err := s.getJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %s does not exist", jobID)
	}
	if _, err := s.requireJobRole(ctx, job, RoleLabelOwner); err != nil {
		return nil, err
	}
	if job.Status != StatusOpen && job.Status != StatusRunning {
		return nil, fmt.Errorf("job %s is %s; epochs can only be logged on OPEN or RUNNING jobs", jobID, job.Status)
	}
	if epoch < 0 {
		return nil, fmt.Errorf("epoch must be non-negative, got %d", epoch)
	}

	// Reject duplicate epoch numbers so totalEpochs stays consistent.
	epochKey, err := ctx.GetStub().CreateCompositeKey(objectTypeEpoch, []string{jobID, fmt.Sprintf("%012d", epoch)})
	if err != nil {
		return nil, err
	}
	if dup, err := ctx.GetStub().GetState(epochKey); err != nil {
		return nil, err
	} else if dup != nil {
		return nil, fmt.Errorf("epoch %d already logged for job %s", epoch, jobID)
	}

	// Read the private loss from transient data.
	transient, err := ctx.GetStub().GetTransient()
	if err != nil {
		return nil, fmt.Errorf("failed to read transient data: %w", err)
	}
	lossRaw, ok := transient["loss"]
	if !ok {
		return nil, fmt.Errorf("transient data must contain key \"loss\"")
	}
	var loss float64
	if err := json.Unmarshal(lossRaw, &loss); err != nil {
		return nil, fmt.Errorf("transient \"loss\" must be a JSON number: %w", err)
	}

	// Canonicalise the private payload, store it privately, and hash it.
	detail := PrivateLossDetail{JobID: jobID, Epoch: epoch, Loss: loss}
	canonical, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().PutPrivateData(PrivateCollection, epochKey, canonical); err != nil {
		return nil, fmt.Errorf("failed to write private loss detail: %w", err)
	}
	sum := sha256.Sum256(canonical)
	lossHash := hex.EncodeToString(sum[:])

	recordedAt, err := txTimestampRFC3339(ctx)
	if err != nil {
		return nil, err
	}

	record := &EpochRecord{
		ObjectType: objectTypeEpoch,
		JobID:      jobID,
		Epoch:      epoch,
		LossHash:   lossHash,
		RecordedAt: recordedAt,
	}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().PutState(epochKey, recordBytes); err != nil {
		return nil, err
	}

	// First epoch flips OPEN -> RUNNING; always bump the counter.
	if job.Status == StatusOpen {
		job.Status = StatusRunning
	}
	job.TotalEpochs++
	if err := s.putJob(ctx, job); err != nil {
		return nil, err
	}

	// Public event carries only the hash — never the raw loss.
	eventBytes, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().SetEvent(EventEpochLogged, eventBytes); err != nil {
		return nil, err
	}
	return record, nil
}

// ---------------------------------------------------------------------------
// 4. FinalizeTraining
// ---------------------------------------------------------------------------

// FinalizeTraining closes a RUNNING job. Only the LABEL_OWNER may call. The job
// must have at least one logged epoch (status RUNNING).
func (s *SmartContract) FinalizeTraining(ctx contractapi.TransactionContextInterface, jobID, finalModelHash string) (*TrainingJob, error) {
	job, err := s.getJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %s does not exist", jobID)
	}
	if _, err := s.requireJobRole(ctx, job, RoleLabelOwner); err != nil {
		return nil, err
	}
	if job.Status != StatusRunning {
		return nil, fmt.Errorf("job %s is %s; only a RUNNING job (>=1 epoch) can be finalized", jobID, job.Status)
	}

	finalizedAt, err := txTimestampRFC3339(ctx)
	if err != nil {
		return nil, err
	}
	job.Status = StatusFinalized
	job.FinalModelHash = finalModelHash
	job.FinalizedAt = finalizedAt

	if err := s.putJob(ctx, job); err != nil {
		return nil, err
	}
	eventBytes, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().SetEvent(EventTrainingFinalized, eventBytes); err != nil {
		return nil, err
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// 5. DistributeRewards
// ---------------------------------------------------------------------------

// DistributeRewards credits on-ledger reward points to every participant of a
// FINALIZED job. Only the INITIATOR may call. Idempotent: a second call is
// rejected via the job.Rewarded flag.
//
// Reward model: points are a tamper-evident on-ledger entitlement. Each
// participant is credited rewardBalance += totalEpochs. Conversion to real
// settlement is an off-chain future step and is intentionally NOT implemented.
func (s *SmartContract) DistributeRewards(ctx contractapi.TransactionContextInterface, jobID string) (*TrainingJob, error) {
	job, err := s.getJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %s does not exist", jobID)
	}
	if _, err := s.requireJobRole(ctx, job, RoleInitiator); err != nil {
		return nil, err
	}
	if job.Status != StatusFinalized {
		return nil, fmt.Errorf("job %s is %s; rewards are only distributed for FINALIZED jobs", jobID, job.Status)
	}
	if job.Rewarded {
		return nil, fmt.Errorf("rewards already distributed for job %s", jobID)
	}

	for _, mspID := range job.Participants {
		p, err := s.getParticipant(ctx, mspID)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("participant %s referenced by job %s is missing", mspID, jobID)
		}
		p.RewardBalance += job.TotalEpochs
		key, err := ctx.GetStub().CreateCompositeKey(objectTypeParticipant, []string{mspID})
		if err != nil {
			return nil, err
		}
		bytes, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if err := ctx.GetStub().PutState(key, bytes); err != nil {
			return nil, err
		}
	}

	job.Rewarded = true
	if err := s.putJob(ctx, job); err != nil {
		return nil, err
	}
	eventBytes, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if err := ctx.GetStub().SetEvent(EventRewardsDistributed, eventBytes); err != nil {
		return nil, err
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// Query (read-only) functions
// ---------------------------------------------------------------------------

// ReadParticipant returns the public Participant record for mspID.
func (s *SmartContract) ReadParticipant(ctx contractapi.TransactionContextInterface, mspID string) (*Participant, error) {
	p, err := s.getParticipant(ctx, mspID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("participant %s does not exist", mspID)
	}
	return p, nil
}

// ReadJob returns the public TrainingJob record for jobID.
func (s *SmartContract) ReadJob(ctx contractapi.TransactionContextInterface, jobID string) (*TrainingJob, error) {
	j, err := s.getJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %s does not exist", jobID)
	}
	return j, nil
}

// ReadEpochPublic returns the PUBLIC EpochRecord (including the loss hash).
func (s *SmartContract) ReadEpochPublic(ctx contractapi.TransactionContextInterface, jobID string, epoch int) (*EpochRecord, error) {
	key, err := ctx.GetStub().CreateCompositeKey(objectTypeEpoch, []string{jobID, fmt.Sprintf("%012d", epoch)})
	if err != nil {
		return nil, err
	}
	data, err := ctx.GetStub().GetState(key)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("epoch %d for job %s does not exist", epoch, jobID)
	}
	var rec EpochRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ReadEpochPrivateLoss returns the PRIVATE loss detail from the collection. It
// only succeeds for collection members; non-members get an error from the peer.
func (s *SmartContract) ReadEpochPrivateLoss(ctx contractapi.TransactionContextInterface, jobID string, epoch int) (*PrivateLossDetail, error) {
	key, err := ctx.GetStub().CreateCompositeKey(objectTypeEpoch, []string{jobID, fmt.Sprintf("%012d", epoch)})
	if err != nil {
		return nil, err
	}
	data, err := ctx.GetStub().GetPrivateData(PrivateCollection, key)
	if err != nil {
		return nil, fmt.Errorf("failed to read private loss (are you a collection member?): %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("private loss for epoch %d of job %s does not exist", epoch, jobID)
	}
	var detail PrivateLossDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// GetJobEpochs returns all public EpochRecords for a job, ordered by epoch
// (the zero-padded key guarantees correct range ordering).
func (s *SmartContract) GetJobEpochs(ctx contractapi.TransactionContextInterface, jobID string) ([]*EpochRecord, error) {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey(objectTypeEpoch, []string{jobID})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var records []*EpochRecord
	for iter.HasNext() {
		kv, err := iter.Next()
		if err != nil {
			return nil, err
		}
		var rec EpochRecord
		if err := json.Unmarshal(kv.Value, &rec); err != nil {
			return nil, err
		}
		records = append(records, &rec)
	}
	return records, nil
}

// ---------------------------------------------------------------------------
// Free helpers
// ---------------------------------------------------------------------------

// parseCSV splits a comma-separated string, trimming whitespace and dropping
// empty entries.
func parseCSV(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// contains reports whether needle is in haystack.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
