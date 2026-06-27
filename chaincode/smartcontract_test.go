package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---------------------------------------------------------------------------
// In-memory test harness (fakes live in fakes_test.go)
// ---------------------------------------------------------------------------

type testEnv struct {
	ctx  *fakeTxContext
	stub *fakeStub
	ci   *fakeClientIdentity
}

func newTestEnv() *testEnv {
	stub := &fakeStub{
		world:     map[string][]byte{},
		priv:      map[string]map[string][]byte{},
		transient: map[string][]byte{},
		ts:        timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	ci := &fakeClientIdentity{}
	ctx := &fakeTxContext{stub: stub, ci: ci}
	return &testEnv{ctx: ctx, stub: stub, ci: ci}
}

func (env *testEnv) setCaller(mspID string) {
	env.ci.mspID = mspID
}

func (env *testEnv) setTransientLoss(loss float64) {
	env.stub.transient = map[string][]byte{"loss": mustJSON(loss)}
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// registerBaseline registers MCI (INITIATOR) and Bank (LABEL_OWNER).
func registerBaseline(t *testing.T, env *testEnv, sc *SmartContract) {
	t.Helper()
	env.setCaller("MCIMSP")
	_, err := sc.RegisterParticipant(env.ctx, "MCIMSP", RoleInitiator, "Hamrah-e Avval (MCI)")
	require.NoError(t, err)
	env.setCaller("BankMSP")
	_, err = sc.RegisterParticipant(env.ctx, "BankMSP", RoleLabelOwner, "Partner Bank")
	require.NoError(t, err)
}

// setupFinalizedJob runs register -> start -> 3 epochs -> finalize. Leaves the
// caller as MCI (initiator) on return.
func setupFinalizedJob(t *testing.T, env *testEnv, sc *SmartContract) {
	t.Helper()
	registerBaseline(t, env, sc)

	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	env.setCaller("BankMSP")
	for i, loss := range []float64{0.9, 0.5, 0.2} {
		env.setTransientLoss(loss)
		_, err := sc.LogEpochResult(env.ctx, "job1", i+1)
		require.NoError(t, err)
	}
	_, err = sc.FinalizeTraining(env.ctx, "job1", "finalmodelhash-abc123")
	require.NoError(t, err)

	env.setCaller("MCIMSP")
}

// ---------------------------------------------------------------------------
// Happy path: full lifecycle
// ---------------------------------------------------------------------------

func TestFullLifecycle(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}

	registerBaseline(t, env, sc)
	require.Equal(t, EventParticipantRegistered, env.stub.lastEventName)

	// Start job (2 participants, multi-party-ready list).
	env.setCaller("MCIMSP")
	job, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)
	require.Equal(t, StatusOpen, job.Status)
	require.Equal(t, []string{"MCIMSP", "BankMSP"}, job.Participants)
	require.Equal(t, EventTrainingJobStarted, env.stub.lastEventName)

	// Log 3 epochs via transient loss; verify hash + privacy each time.
	env.setCaller("BankMSP")
	losses := []float64{0.9, 0.5, 0.2}
	for i, loss := range losses {
		env.setTransientLoss(loss)
		rec, err := sc.LogEpochResult(env.ctx, "job1", i+1)
		require.NoError(t, err)

		expected := PrivateLossDetail{JobID: "job1", Epoch: i + 1, Loss: loss}
		sum := sha256.Sum256(mustJSON(expected))
		require.Equal(t, hex.EncodeToString(sum[:]), rec.LossHash, "lossHash must equal SHA-256 of canonical private payload")

		// Public event must carry the hash but NOT the raw loss.
		require.Equal(t, EventEpochLogged, env.stub.lastEventName)
		var evt map[string]interface{}
		require.NoError(t, json.Unmarshal(env.stub.lastEventPayload, &evt))
		require.Contains(t, evt, "lossHash")
		_, hasLoss := evt["loss"]
		require.False(t, hasLoss, "event payload must never contain the raw loss")
	}

	// First epoch flipped status to RUNNING and counter is 3.
	rj, err := sc.ReadJob(env.ctx, "job1")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, rj.Status)
	require.Equal(t, 3, rj.TotalEpochs)

	// Finalize (label owner).
	env.setCaller("BankMSP")
	fj, err := sc.FinalizeTraining(env.ctx, "job1", "finalmodelhash-abc123")
	require.NoError(t, err)
	require.Equal(t, StatusFinalized, fj.Status)
	require.Equal(t, "finalmodelhash-abc123", fj.FinalModelHash)
	require.Equal(t, EventTrainingFinalized, env.stub.lastEventName)

	// Distribute rewards (initiator).
	env.setCaller("MCIMSP")
	dj, err := sc.DistributeRewards(env.ctx, "job1")
	require.NoError(t, err)
	require.True(t, dj.Rewarded)
	require.Equal(t, EventRewardsDistributed, env.stub.lastEventName)

	// Each participant credited totalEpochs (3).
	mci, err := sc.ReadParticipant(env.ctx, "MCIMSP")
	require.NoError(t, err)
	require.Equal(t, 3, mci.RewardBalance)
	bank, err := sc.ReadParticipant(env.ctx, "BankMSP")
	require.NoError(t, err)
	require.Equal(t, 3, bank.RewardBalance)

	// Public epoch record carries hash only.
	pub, err := sc.ReadEpochPublic(env.ctx, "job1", 2)
	require.NoError(t, err)
	require.NotEmpty(t, pub.LossHash)
	require.NotContains(t, string(mustJSON(pub)), "\"loss\":")

	// Private payload round-trips for a member.
	env.setCaller("BankMSP")
	pl, err := sc.ReadEpochPrivateLoss(env.ctx, "job1", 2)
	require.NoError(t, err)
	require.Equal(t, 0.5, pl.Loss)

	// Range query returns epochs in order.
	eps, err := sc.GetJobEpochs(env.ctx, "job1")
	require.NoError(t, err)
	require.Len(t, eps, 3)
	require.Equal(t, 1, eps[0].Epoch)
	require.Equal(t, 2, eps[1].Epoch)
	require.Equal(t, 3, eps[2].Epoch)
}

// ---------------------------------------------------------------------------
// Access-control rejections
// ---------------------------------------------------------------------------

func TestStartJob_NonInitiatorRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)

	// Bank is LABEL_OWNER, not INITIATOR.
	env.setCaller("BankMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.Error(t, err)
	require.Contains(t, err.Error(), "INITIATOR is required")
}

func TestLogEpoch_NonLabelOwnerRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	// MCI (initiator, not label owner) tries to log an epoch.
	env.setCaller("MCIMSP")
	env.setTransientLoss(0.4)
	_, err = sc.LogEpochResult(env.ctx, "job1", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not the LABEL_OWNER")
}

func TestDistribute_NonInitiatorRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	setupFinalizedJob(t, env, sc)

	// Bank (label owner) tries to distribute.
	env.setCaller("BankMSP")
	_, err := sc.DistributeRewards(env.ctx, "job1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not the INITIATOR")
}

// ---------------------------------------------------------------------------
// State-machine rejections
// ---------------------------------------------------------------------------

func TestLogEpoch_NonExistentJobRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)

	env.setCaller("BankMSP")
	env.setTransientLoss(0.4)
	_, err := sc.LogEpochResult(env.ctx, "ghost", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
}

func TestLogEpoch_FinalizedJobRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	setupFinalizedJob(t, env, sc)

	env.setCaller("BankMSP")
	env.setTransientLoss(0.4)
	_, err := sc.LogEpochResult(env.ctx, "job1", 99)
	require.Error(t, err)
	require.Contains(t, err.Error(), "FINALIZED")
}

func TestFinalize_ZeroEpochsRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	// Job is OPEN (no epochs) -> finalize must fail.
	env.setCaller("BankMSP")
	_, err = sc.FinalizeTraining(env.ctx, "job1", "hash")
	require.Error(t, err)
	require.Contains(t, err.Error(), "RUNNING")
}

func TestDistribute_NonFinalizedRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	// Job still OPEN -> distribute must fail.
	env.setCaller("MCIMSP")
	_, err = sc.DistributeRewards(env.ctx, "job1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "FINALIZED")
}

func TestDistribute_DoubleDistributionRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	setupFinalizedJob(t, env, sc)

	env.setCaller("MCIMSP")
	_, err := sc.DistributeRewards(env.ctx, "job1")
	require.NoError(t, err)

	// Second distribution is rejected (idempotency).
	_, err = sc.DistributeRewards(env.ctx, "job1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already distributed")

	// Balances were not double-credited.
	bank, err := sc.ReadParticipant(env.ctx, "BankMSP")
	require.NoError(t, err)
	require.Equal(t, 3, bank.RewardBalance)
}

// ---------------------------------------------------------------------------
// Additional validation rejections
// ---------------------------------------------------------------------------

func TestRegister_NotSelfRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	env.setCaller("MCIMSP")
	_, err := sc.RegisterParticipant(env.ctx, "BankMSP", RoleLabelOwner, "Partner Bank")
	require.Error(t, err)
	require.Contains(t, err.Error(), "self-register")
}

func TestRegister_DuplicateRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	env.setCaller("MCIMSP")
	_, err := sc.RegisterParticipant(env.ctx, "MCIMSP", RoleInitiator, "MCI")
	require.NoError(t, err)
	_, err = sc.RegisterParticipant(env.ctx, "MCIMSP", RoleInitiator, "MCI")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already registered")
}

func TestRegister_InvalidRoleRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	env.setCaller("MCIMSP")
	_, err := sc.RegisterParticipant(env.ctx, "MCIMSP", "OVERLORD", "MCI")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid role")
}

func TestStartJob_LabelOwnerMustHoldRole(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	// MCI initiator, Bank registered as CONTRIBUTOR (not LABEL_OWNER).
	env.setCaller("MCIMSP")
	_, err := sc.RegisterParticipant(env.ctx, "MCIMSP", RoleInitiator, "MCI")
	require.NoError(t, err)
	env.setCaller("BankMSP")
	_, err = sc.RegisterParticipant(env.ctx, "BankMSP", RoleContributor, "Bank")
	require.NoError(t, err)

	env.setCaller("MCIMSP")
	_, err = sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a LABEL_OWNER")
}

func TestStartJob_RequiresTwoParticipants(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP", "MCIMSP")
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least 2 participants")
}

func TestLogEpoch_DuplicateEpochRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	env.setCaller("BankMSP")
	env.setTransientLoss(0.5)
	_, err = sc.LogEpochResult(env.ctx, "job1", 1)
	require.NoError(t, err)

	env.setTransientLoss(0.4)
	_, err = sc.LogEpochResult(env.ctx, "job1", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already logged")
}

func TestLogEpoch_MissingTransientRejected(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	env.setCaller("BankMSP")
	env.stub.transient = map[string][]byte{} // no "loss"
	_, err = sc.LogEpochResult(env.ctx, "job1", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "loss")
}

// ---------------------------------------------------------------------------
// Privacy: non-member cannot read the private loss
// ---------------------------------------------------------------------------

func TestPrivacy_NonMemberCannotReadLoss(t *testing.T) {
	env := newTestEnv()
	sc := &SmartContract{}
	registerBaseline(t, env, sc)
	env.setCaller("MCIMSP")
	_, err := sc.StartTrainingJob(env.ctx, "job1", "MCIMSP,BankMSP", "BankMSP")
	require.NoError(t, err)

	env.setCaller("BankMSP")
	env.setTransientLoss(0.42)
	_, err = sc.LogEpochResult(env.ctx, "job1", 1)
	require.NoError(t, err)

	// Member reads fine.
	pl, err := sc.ReadEpochPrivateLoss(env.ctx, "job1", 1)
	require.NoError(t, err)
	require.Equal(t, 0.42, pl.Loss)

	// Simulate a non-member peer: the collection rejects the read.
	env.stub.nonMember = true
	_, err = sc.ReadEpochPrivateLoss(env.ctx, "job1", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "read access")

	// And the raw loss never landed on the PUBLIC ledger.
	pub, err := sc.ReadEpochPublic(env.ctx, "job1", 1)
	require.NoError(t, err)
	require.NotContains(t, string(mustJSON(pub)), "0.42")
}
