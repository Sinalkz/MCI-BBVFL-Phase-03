# Claude Code Prompt — Phase 3: Hyperledger Fabric Chaincode for a Blockchain-Audited VFL Platform

> Paste everything below into Claude Code as the task. It is written to be self-contained.
> Build target: **real Go chaincode**, validated against Fabric's **contract test framework / unit tests** (MockStub-style), **NOT** a full multi-org Docker network. Deployment to a live network is explicitly out of scope for this task.

---

## 1. Context (read first, do not skip)

You are building the smart-contract (chaincode) layer for a research proof-of-concept. The wider system lets a telecom operator (**MCI**) jointly train a **Vertical Federated Learning (VFL)** logistic-regression model with one or more data-holding partners (a **Bank** today; **Digikala** is a planned third party) to predict short-term loan eligibility for shared users — **without any party sharing raw data**. The actual VFL training (numpy-based) and the wiring of training events to this chaincode happen in a later phase. **Your job is only the chaincode and its tests.**

The chaincode is the **audit, identity, and reward layer**. A previous proof-of-concept logged training events to an in-memory Python list (`log_event(...)`). You are replacing that simulator with real Fabric chaincode. The five Python `log_event`-style calls become five chaincode transaction functions.

**Platform decision (already made, do not revisit):** Hyperledger **Fabric** (permissioned/consortium), chaincode in **Go**, using the **`fabric-contract-api-go/v2`** programming model. Rationale: the system is an explicitly *consortium* setting (a few known, semi-trusted orgs), which is what Fabric is for; Ethereum/Solidity would contradict that and bring a public-by-default, gas/token model we don't want.

## 2. What to build

A single chaincode package (Go module) implementing one `SmartContract` (embedding `contractapi.Contract`) with the data model, functions, access control, events, and a private data collection described below. Plus a collections config JSON and a full unit-test suite.

### 2.1 Module / dependency requirements

- Go module using **`github.com/hyperledger/fabric-contract-api-go/v2/contractapi`**.
- For the chaincode shim / stub types use **`github.com/hyperledger/fabric-chaincode-go/v2`** (the v2 path — required to avoid the `fabric-protos-go` protobuf namespace conflict; do **not** import the old non-v2 `fabric-protos-go`).
- Provide a working `go.mod`. Pin to currently-resolving versions; if a version fails to resolve, pick the nearest working release and note it in a comment.
- `main()` creates the chaincode from the contract via `contractapi.NewChaincode(...)` and starts it.
- Use `ctx.GetClientIdentity().GetMSPID()` for caller identity, and `ctx.GetStub()` for ledger + private-data + events.

### 2.2 Data model

The job model is **multi-party-ready by design** but will be exercised with two participants (MCI + Bank). Do **not** hard-code exactly two org slots — a job holds a **list** of participant references.

Public ledger state (world state), each as a JSON struct with an `objectType` discriminator field and a composite key:

**Participant**
```
mspID        string   // e.g. "MCIMSP" — also the key
role         string   // one of: "INITIATOR" | "LABEL_OWNER" | "CONTRIBUTOR"
displayName  string   // e.g. "Hamrah-e Avval (MCI)"
registeredAt string   // RFC3339; derive from tx timestamp, NOT time.Now()
rewardBalance int     // accumulated, tamper-evident reward points (see 2.4)
```
Key: `CreateCompositeKey("participant", []string{mspID})`.

**TrainingJob**
```
jobId         string
status        string   // "OPEN" | "RUNNING" | "FINALIZED"
initiatorMSP  string   // the INITIATOR who created the job (MCI)
participants  []string // list of participant mspIDs in this job (>=2). Exactly one must be LABEL_OWNER.
labelOwnerMSP string   // which participant holds the label Y
totalEpochs   int      // count of epochs logged so far
finalModelHash string  // set on finalize
createdAt      string  // from tx timestamp
finalizedAt    string  // from tx timestamp, set on finalize
```
Key: `CreateCompositeKey("job", []string{jobId})`.

**EpochRecord (public part)** — the audit anchor. The sensitive loss value is NOT here; only its hash.
```
jobId      string
epoch      int
lossHash   string   // SHA-256 hex of the private loss payload (see PDC below)
recordedAt string   // from tx timestamp
```
Key: `CreateCompositeKey("epoch", []string{jobId, fmt.Sprintf("%012d", epoch)})` (zero-pad epoch so range queries sort correctly).

### 2.3 Private Data Collection (the privacy element)

Define **one** private data collection named **`collectionLossDetails`**, shared by the job's participant orgs (in the 2-party test case: MCI + Bank). Provide a `collections_config.json` with this collection (memberOrgsPolicy as an OR over the two test MSPs, `requiredPeerCount` 0, `maxPeerCount` 1, `blockToLive` 0 = never purge, `memberOnlyRead` true, `memberOnlyWrite` true).

The **actual loss value** for each epoch is private:
```
PrivateLossDetail {
  jobId   string
  epoch   int
  loss    float64   // the real loss — visible ONLY to collection members
}
```
- `logEpochResult` reads the private payload from the **transient** field (`ctx.GetStub().GetTransient()`), writes it to the collection via `PutPrivateData("collectionLossDetails", key, bytes)`, computes `SHA-256` over the canonical JSON bytes, and stores that hash in the **public** `EpochRecord.lossHash`.
- This is the core privacy story: **the audit trail (epoch happened, here's its hash) is public; the actual loss number is private to the participants; the hash links them and makes tampering detectable.**

### 2.4 The five functions (these replace the Python `log_event` calls)

For every state-changing function: validate caller identity/role first, validate state transitions, write state, **emit an event** via `ctx.GetStub().SetEvent(name, payloadJSON)`. Derive all timestamps from `ctx.GetStub().GetTxTimestamp()` (deterministic — never `time.Now()`, which breaks endorsement). Return typed structs, not raw bytes, where the contract-api allows.

1. **`RegisterParticipant(ctx, mspID, role, displayName)`**
   - Caller's own MSPID must equal `mspID` (an org registers itself), OR caller is the INITIATOR registering others — pick self-registration as the rule and enforce `ctx.GetClientIdentity().GetMSPID() == mspID`.
   - `role` must be one of the three valid roles. Reject duplicates. Init `rewardBalance = 0`. Emit `ParticipantRegistered`.

2. **`StartTrainingJob(ctx, jobId, participantsCSV, labelOwnerMSP)`**
   - Only a caller whose registered role is **INITIATOR** may call. (Look up caller's Participant record; reject if not INITIATOR.)
   - Parse `participantsCSV` into a list; require length >= 2; require all are registered Participants; require `labelOwnerMSP` is in the list and its Participant role is `LABEL_OWNER`; require the initiator is in the list.
   - Reject if `jobId` already exists. Set status `OPEN`. Emit `TrainingJobStarted`.

3. **`LogEpochResult(ctx, jobId, epoch)`** — note: the loss comes via **transient data**, not as a plain arg.
   - Only the job's **LABEL_OWNER** may call (it's the party that computes loss against the label). Enforce `caller MSPID == job.labelOwnerMSP`.
   - Job must exist and be `OPEN` or `RUNNING`; flip to `RUNNING` on first epoch.
   - Read `loss` from transient (`GetTransient()["loss"]`), build `PrivateLossDetail`, `PutPrivateData` to `collectionLossDetails`, compute SHA-256 hash, write public `EpochRecord{lossHash}`, increment `job.totalEpochs`. Emit `EpochLogged` (public payload: jobId, epoch, lossHash — **never the raw loss**).

4. **`FinalizeTraining(ctx, jobId, finalModelHash)`**
   - Only the job's **LABEL_OWNER** may call. Job must be `RUNNING` (at least one epoch logged). Set status `FINALIZED`, store `finalModelHash` and `finalizedAt`. Emit `TrainingFinalized`.

5. **`DistributeRewards(ctx, jobId)`**
   - Only the job's **INITIATOR** may call. Job must be `FINALIZED` (you don't pay for incomplete work).
   - Reward model = **on-ledger points balance** (tamper-evident entitlement; conversion to real settlement is an off-chain future step — document this, don't implement payment). For each participant in the job, credit `rewardBalance += totalEpochs` (simple, ledger-computable contribution metric: participation in a finalized job weighted by epochs recorded). Make it **idempotent** — guard against double-distribution by setting a `rewarded` flag on the job and rejecting a second call. Emit `RewardsDistributed`.

### 2.5 Query (read-only) functions

Add read functions (no role gate, but respect PDC membership for the private one):
- `ReadParticipant(ctx, mspID)`, `ReadJob(ctx, jobId)`, `ReadEpochPublic(ctx, jobId, epoch)` (returns the public EpochRecord incl. hash).
- `ReadEpochPrivateLoss(ctx, jobId, epoch)` — `GetPrivateData` from the collection; only succeeds for collection members. Demonstrates that non-members cannot read the loss.
- `GetJobEpochs(ctx, jobId)` — range query over the epoch composite keys for a job.

### 2.6 Access-control helper

Write one helper, e.g. `requireRole(ctx, expectedRole)` and `requireJobRole(ctx, job, field)`, that reads the caller MSPID and the relevant Participant/Job record and returns a clear error on mismatch. All five functions use it. Errors must be explicit and testable (e.g. `fmt.Errorf("caller %s is not the LABEL_OWNER for job %s", caller, jobId)`).

## 3. Tests (this is how Phase 3 is "validated" — take it seriously)

Use Go unit tests with a **mocked transaction context and stub** (generate mocks with `counterfeiter` against the contract-api interfaces, following the pattern in the official `fabric-samples` `asset-transfer-basic`/`asset-transfer-private-data` Go chaincode tests, OR hand-write fakes if simpler). Cover:

- **Happy path, full lifecycle:** register MCI (INITIATOR) + Bank (LABEL_OWNER) → start job (2 participants) → log 3 epochs (loss via transient) → finalize → distribute rewards. Assert state transitions, that `lossHash` is set and equals SHA-256 of the private payload, that `rewardBalance` updated, and that the public EpochRecord never contains the raw loss.
- **Access-control rejections:** non-INITIATOR starting a job is rejected; a participant who is not the LABEL_OWNER logging an epoch is rejected; non-INITIATOR distributing rewards is rejected.
- **State-machine rejections:** logging on a non-existent/finalized job; finalizing a job with zero epochs; distributing on a non-finalized job; double `DistributeRewards` is rejected (idempotency).
- **Privacy:** the public EpochRecord and the `EpochLogged` event payload contain only the hash, never the loss; assert the private payload round-trips via the mocked private-data store.

Aim for these to actually run with `go test ./...` and pass.

## 4. Deliverables / repo layout

```
chaincode/
  go.mod
  go.sum
  main.go
  smartcontract.go        // the SmartContract + 5 funcs + queries + helpers
  models.go               // Participant, TrainingJob, EpochRecord, PrivateLossDetail
  smartcontract_test.go   // the test suite
  collections_config.json // the PDC definition
  README.md               // how to run tests; how this maps to the 5 Phase-2 log_event calls; what is in/out of scope (no live network); the multi-party-ready note (Digikala as future 3rd participant); the reward model (on-ledger entitlement, off-chain settlement = future work)
mocks/                    // generated or hand-written test doubles, if separate
```

## 5. Constraints & non-goals (respect these)

- **Do NOT** stand up a Docker/multi-org `test-network`, write Ansible/compose for peers/orderers, or attempt `peer lifecycle` deployment. Tests run against mocks only.
- **Do NOT** import the deprecated non-v2 `fabric-protos-go`. Use v2 module paths.
- **Do NOT** use `time.Now()` anywhere in chaincode logic — only `GetTxTimestamp()` (determinism).
- **Do NOT** put the raw loss on the public ledger or in any event payload.
- **Do NOT** implement real token transfer / payment rails — rewards are on-ledger points + a documented off-chain settlement note.
- Keep functions deterministic and endorsement-safe. Validate all inputs. Return clear, testable errors.
- After writing, run `go vet` and `go test ./...`; fix until green; in the README report the exact versions you pinned and anything you had to adjust.

## 6. When done, report back

Summarize: (a) the final `go.mod` versions used, (b) `go test` output (pass/fail counts), (c) any deviation from this spec and why, (d) anything that would need to change to go from mock-tested chaincode to a deployed multi-org network. This summary feeds the Phase 3 report.
