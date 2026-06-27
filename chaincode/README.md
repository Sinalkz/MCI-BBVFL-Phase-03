# VFL Audit Chaincode — Phase 3

Hyperledger Fabric chaincode (Go, `fabric-contract-api-go/v2`) that is the
**audit, identity, and reward layer** for a Vertical Federated Learning (VFL)
proof-of-concept. A telecom operator (**MCI**) jointly trains a logistic-regression
model with a data-holding partner (a **Bank**) to predict loan eligibility for
shared users — *without either party sharing raw data*. This chaincode replaces a
Phase-2 Python simulator that logged training events to an in-memory list.

> **Scope:** real Go chaincode validated against **unit tests with hand-written
> fakes** (MockStub-style). Standing up a multi-org Docker `test-network` and
> deploying to a live network is **out of scope** (see *In / out of scope*).

## How this maps to the Phase-2 `log_event` calls

The five Python `log_event(...)`-style calls become five chaincode transaction
functions (each validates identity/role + state, writes state, emits an event,
and uses deterministic `GetTxTimestamp()` timestamps):

| Phase-2 simulator call            | Phase-3 chaincode function                       | Caller role  |
| --------------------------------- | ------------------------------------------------ | ------------ |
| register a party                  | `RegisterParticipant(mspID, role, displayName)`  | self         |
| start a training job              | `StartTrainingJob(jobId, participantsCSV, labelOwnerMSP)` | INITIATOR |
| log one epoch's loss              | `LogEpochResult(jobId, epoch)` *(loss via transient)* | LABEL_OWNER |
| finalize / publish final model    | `FinalizeTraining(jobId, finalModelHash)`        | LABEL_OWNER  |
| reward contributors               | `DistributeRewards(jobId)`                        | INITIATOR    |

Read-only helpers: `ReadParticipant`, `ReadJob`, `ReadEpochPublic`,
`ReadEpochPrivateLoss` (collection-members only), `GetJobEpochs` (range query).

## The privacy story (private data collection)

- A single PDC, **`collectionLossDetails`**, is shared by the job's participant
  orgs (test case: `MCIMSP` + `BankMSP`). See [`collections_config.json`](collections_config.json).
- `LogEpochResult` reads the real `loss` from **transient data** (never as a plain
  argument), writes a `PrivateLossDetail` into the collection via `PutPrivateData`,
  computes **SHA-256** over its canonical JSON, and stores **only that hash** in the
  public `EpochRecord.lossHash`.
- Result: the **audit trail is public** (an epoch happened; here's its hash), the
  **loss value is private** to the participants, and the hash links them so any
  tampering with the private value is detectable. The raw loss never appears on the
  public ledger or in any event payload (asserted by the tests).

## Data model

- **Participant** — `mspID` (key), `role` (`INITIATOR` | `LABEL_OWNER` | `CONTRIBUTOR`),
  `displayName`, `registeredAt`, `rewardBalance`.
- **TrainingJob** — `jobId` (key), `status` (`OPEN` | `RUNNING` | `FINALIZED`),
  `initiatorMSP`, **`participants []string`** (multi-party-ready — *not* two fixed
  slots), `labelOwnerMSP`, `totalEpochs`, `finalModelHash`, timestamps, `rewarded`.
- **EpochRecord** (public) — `jobId`, `epoch`, `lossHash`, `recordedAt`. Key zero-pads
  the epoch (`%012d`) so range queries sort numerically.
- **PrivateLossDetail** (private) — `jobId`, `epoch`, `loss`. Lives only in the PDC.

### Multi-party-ready note

The job holds a **list** of participants, not a hard-coded pair. The 2-party test
exercises MCI + Bank, but adding a planned third party (**Digikala**) is just a
longer `participantsCSV` plus adding its MSP to the collection policy — no model
change. Exactly one participant must be the `LABEL_OWNER`.

### Reward model

`DistributeRewards` credits each participant `rewardBalance += totalEpochs`: a
simple, ledger-computable contribution metric (participation in a finalized job,
weighted by epochs recorded). These are **on-ledger entitlement points** — a
tamper-evident record, **not** a token transfer. Converting points to real
settlement is a deliberate **off-chain, future step** and is *not* implemented here.
The call is **idempotent**: a `rewarded` flag on the job rejects any second call.

## How to run and use the project

Since this chaincode has no live network (by design — see *In / out of scope*),
**running it means running the unit tests**. The tests are the primary deliverable
and exercise every transaction function end-to-end with an in-memory ledger.

### Prerequisites

Go 1.26+ must be installed. Verify with:

```powershell
go version
# expected: go version go1.26.4 windows/amd64
```

### 1. Navigate to the chaincode directory

```powershell
cd "e:\01_Education\02_Masters\00_Semesters\Term 6\Thesis\03_Mci\Report_Phase_03\mci_codebase_phase03\chaincode"
```

### 2. Static analysis

```powershell
go vet ./...
# no output = clean
```

### 3. Run all tests (verbose)

```powershell
go test ./... -v -count=1
```

Expected output:

```
=== RUN   TestFullLifecycle
--- PASS: TestFullLifecycle (0.00s)
=== RUN   TestStartJob_NonInitiatorRejected
--- PASS: TestStartJob_NonInitiatorRejected (0.00s)
...
PASS
ok      vfl-audit-chaincode     1.2s
```

### 4. Run a single test by name

```powershell
go test ./... -run TestFullLifecycle -v
```

### 5. Build the chaincode binary

```powershell
go build ./...
# BUILD OK = compiles cleanly
```

This compiles the binary but does not start it — starting requires a live Fabric
peer to connect to, which is out of scope for Phase 3.

### What each test exercises

| Test | What it proves |
| --- | --- |
| `TestFullLifecycle` | Full register → start → 3 epochs → finalize → distribute lifecycle |
| `TestStartJob_NonInitiatorRejected` | Only INITIATOR can start a job |
| `TestLogEpoch_NonLabelOwnerRejected` | Only LABEL_OWNER can log epochs |
| `TestDistribute_NonInitiatorRejected` | Only INITIATOR can distribute rewards |
| `TestLogEpoch_NonExistentJobRejected` | Cannot log to a non-existent job |
| `TestLogEpoch_FinalizedJobRejected` | Cannot log to a finalized job |
| `TestFinalize_ZeroEpochsRejected` | Cannot finalize with zero epochs |
| `TestDistribute_NonFinalizedRejected` | Cannot reward an unfinished job |
| `TestDistribute_DoubleDistributionRejected` | Idempotency guard rejects a second distribution |
| `TestRegister_NotSelfRejected` | Only self-registration is allowed |
| `TestRegister_DuplicateRejected` | Cannot register the same MSP twice |
| `TestRegister_InvalidRoleRejected` | Role must be one of the three valid values |
| `TestStartJob_LabelOwnerMustHoldRole` | Declared `labelOwnerMSP` must actually hold LABEL_OWNER role |
| `TestStartJob_RequiresTwoParticipants` | Jobs require ≥ 2 participants |
| `TestLogEpoch_DuplicateEpochRejected` | Cannot log the same epoch number twice |
| `TestLogEpoch_MissingTransientRejected` | Loss must arrive via transient data, not as a plain argument |
| `TestPrivacy_NonMemberCannotReadLoss` | Non-member peer is refused the private loss |

### Current test status (last run)

- `go vet ./...` — clean.
- `go build ./...` — OK.
- `go test ./...` — **17 / 17 PASS** (`ok vfl-audit-chaincode`).

Tests cover: the full happy-path lifecycle (register → start → 3 epochs → finalize
→ distribute) with hash/privacy/reward assertions; access-control rejections
(non-INITIATOR start, non-LABEL_OWNER log, non-INITIATOR distribute); state-machine
rejections (log on missing/finalized job, finalize with zero epochs, distribute on
non-finalized job, double-distribute idempotency); input validation
(self-registration, duplicates, invalid role, <2 participants, label-owner role,
duplicate epoch, missing transient); and privacy (public record/event carry only
the hash, private payload round-trips, a non-member peer is refused the loss).

### What "running" would look like on a real network (out of scope here)

To deploy this to a live Fabric network you would need:

1. A running `test-network` (Docker — two orgs, an orderer, a channel).
2. `peer lifecycle chaincode package/install/approveformyorg/commit --collections-config collections_config.json`
3. A client (Go/Node Fabric Gateway SDK) submitting transactions such as
   `RegisterParticipant`, `StartTrainingJob`, etc., with the `loss` value passed via
   the transient map on `LogEpochResult` calls.

This is explicitly out of scope for Phase 3 — the tests are the deliverable.

## Pinned versions (resolved on this machine)

Built with **Go 1.26.4 (windows/amd64)**.

| Module                                              | Version   |
| --------------------------------------------------- | --------- |
| `github.com/hyperledger/fabric-contract-api-go/v2`  | `v2.2.0`  |
| `github.com/hyperledger/fabric-chaincode-go/v2`     | `v2.3.0`  |
| `github.com/hyperledger/fabric-protos-go-apiv2`     | `v0.3.7`  |
| `google.golang.org/protobuf`                        | `v1.36.5` |
| `github.com/stretchr/testify`                       | `v1.11.1` |

> `go get ...@latest` initially proposed `fabric-contract-api-go/v2 v2.2.1` and a
> pseudo-version of `fabric-chaincode-go/v2`; the module graph's
> minimal-version-selection settled on the **v2.2.0 / v2.3.0** pair above, which
> resolve and build cleanly together. The deprecated non-v2 `fabric-protos-go` is
> **not** imported anywhere (only `fabric-protos-go-apiv2`), avoiding the protobuf
> namespace conflict.

## Files

```
chaincode/
  go.mod / go.sum          # module: vfl-audit-chaincode; pinned deps above
  main.go                  # contractapi.NewChaincode(...) + Start()
  models.go                # Participant, TrainingJob, EpochRecord, PrivateLossDetail + constants
  smartcontract.go         # SmartContract: 5 tx funcs + 5 queries + access-control helpers
  smartcontract_test.go    # the test suite (in-package, package main)
  fakes_test.go            # hand-written in-memory fakes for stub/ctx/identity/iterator
  collections_config.json  # the collectionLossDetails PDC definition
  README.md
```

## In / out of scope

**In scope:** the chaincode, its data model, access control, events, the private
data collection + config, and a unit-test suite that runs `go test ./...` against
in-memory fakes.

**Out of scope (explicitly):** no Docker/multi-org `test-network`, no
peer/orderer compose or Ansible, no `peer lifecycle` packaging/deploy, no real
token/payment rails. `time.Now()` is never used in chaincode logic (only
`GetTxTimestamp()` — determinism for endorsement).

## Deviations from the spec

1. **Test doubles are hand-written fakes, not `counterfeiter`-generated mocks.**
   The spec allowed either ("OR hand-write fakes if simpler"). Counterfeiter
   (v6.12.2) failed to parse the large `ChaincodeStubInterface` under Go 1.26, so the
   fakes in [`fakes_test.go`](fakes_test.go) embed the upstream interfaces and
   override only the methods the chaincode calls — fully self-contained, no codegen
   step or external mock tooling. They therefore live in-package rather than in a
   separate `mocks/` directory.
2. **Duplicate-epoch guard added.** `LogEpochResult` rejects re-logging an
   already-recorded epoch number, keeping `totalEpochs` consistent. This is a
   sensible state-machine validation beyond the literal spec; it has a dedicated test.

## From mock-tested chaincode to a deployed multi-org network

What would need to change/added to go live:

- **A real Fabric network:** ≥2 orgs (MCI, Bank) each with peers + a CA, an orderer
  (Raft), a channel, and TLS — via `fabric-samples test-network` or equivalent.
- **Chaincode lifecycle:** `peer lifecycle chaincode package/install/approveformyorg/commit`,
  with the collection config supplied at approve/commit time (`--collections-config
  collections_config.json`) and a real **endorsement policy** (e.g. AND/OR over the
  org MSPs).
- **Real MSP identities:** `GetMSPID()`/`GetClientIdentity()` would return CA-issued
  identities instead of fakes; the self-registration and role checks would run against
  actual org MSP IDs (`MCIMSP`, `BankMSP`, …).
- **Transient & private-data plumbing:** the VFL training client (later phase) must
  submit `loss` via the transient map and target the right endorsing peers so the
  PDC is written on member peers; non-member orgs would genuinely be unable to read it.
- **Client/SDK layer:** a Fabric Gateway client (Go/Node) to submit transactions and
  subscribe to the emitted events (`EpochLogged`, etc.) for off-chain dashboards.
- **Off-chain settlement:** a process that reads `rewardBalance` and performs the
  real-world payout — the intentional next step the points model defers.
- **Operational hardening:** CouchDB state DB if rich queries are needed, key-level
  endorsement if desired, monitoring, and chaincode upgrade procedures.
```
