# Phase 3 — Verification Answers

Answers to the five Phase-3 verification questions, with quoted lines from
[`chaincode/smartcontract.go`](chaincode/smartcontract.go). Verified against the
actual source (build OK, `go vet` clean, 17/17 tests pass).

---

## 1. Hash determinism — ✅ struct (stable field order), not a map; same bytes hashed and stored

`smartcontract.go:343-353`:

```go
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
```

- The SHA-256 is taken over `canonical = json.Marshal(PrivateLossDetail{...})` — a
  **struct**, so JSON field order is fixed by the struct definition
  (`jobId`, `epoch`, `loss`). **No map** appears anywhere in the hashing path, so
  there is no non-deterministic key-ordering source.
- The **exact same `canonical` bytes** are what `PutPrivateData` writes to the
  private collection. Therefore the public `EpochRecord.lossHash` provably
  corresponds to the private payload stored in `collectionLossDetails`.
- The test re-marshals the same struct and asserts
  `hex(sha256(json.Marshal(PrivateLossDetail{...}))) == rec.LossHash`.

**Verdict:** deterministic and provably linked. No flag.

---

## 2. Float determinism — raw `float64` JSON, not fixed precision (flagged, not changed)

`smartcontract.go:338-344`:

```go
var loss float64
if err := json.Unmarshal(lossRaw, &loss); err != nil {
    return nil, fmt.Errorf("transient \"loss\" must be a JSON number: %w", err)
}
...
detail := PrivateLossDetail{JobID: jobID, Epoch: epoch, Loss: loss}
```

- `loss` is a `float64`, serialized as the **raw float** inside the struct marshal.
  There is **no** `strconv.FormatFloat`-to-fixed-precision step before hashing/storing.
- Go's `encoding/json` emits the shortest round-trip representation, which **is
  deterministic across endorsing peers running the same chaincode binary** — so
  endorsement consensus on the hash is safe.
- **Caveat:** the hash is *not* trivially reproducible by a **non-Go verifier**
  (e.g. a Python auditor recomputing it) unless that verifier matches Go's float
  formatting exactly.

**Committee-grade improvement (noted, not applied):** format to fixed precision
before building `detail`, e.g.
`strconv.FormatFloat(loss, 'f', 8, 64)`, and hash a canonical string form.

**Verdict:** raw float64 — noted as a known reproducibility caveat; left unchanged.

---

## 3. Reward metric — ✅ uniform; no role differentiation

`smartcontract.go:472-477`:

```go
for _, mspID := range job.Participants {
    p, err := s.getParticipant(ctx, mspID)
    if err != nil {
        return nil, err
    }
    p.RewardBalance += job.TotalEpochs
```

- Every participant in `job.Participants` is credited the **same** amount,
  `+= totalEpochs`.
- Initiator, label-owner, and contributors are treated **identically** — a flat,
  ledger-computable metric.

**Verdict:** confirmed uniform. Document as a known PoC limitation.

---

## 4. Transient leakage — ✅ the real value lives only in `collectionLossDetails`

- **Only `LogEpochResult` reads transient.** It is the sole `GetTransient()` call in
  the codebase — `smartcontract.go:330`:

  ```go
  transient, err := ctx.GetStub().GetTransient()
  ```

- The raw `loss` flows into `detail` → `canonical` → **`PutPrivateData` only**
  (`smartcontract.go:349`):

  ```go
  if err := ctx.GetStub().PutPrivateData(PrivateCollection, epochKey, canonical); err != nil {
  ```

- The **public** write stores `record` (an `EpochRecord`), whose only loss-derived
  field is `LossHash` — `smartcontract.go:360-371`:

  ```go
  record := &EpochRecord{
      ObjectType: objectTypeEpoch,
      JobID:      jobID,
      Epoch:      epoch,
      LossHash:   lossHash,
      RecordedAt: recordedAt,
  }
  recordBytes, err := json.Marshal(record)
  ...
  if err := ctx.GetStub().PutState(epochKey, recordBytes); err != nil {
  ```

  `EpochRecord` has **no loss field**, so the raw value cannot appear on the public
  ledger.
- The only function that returns the real value is `ReadEpochPrivateLoss`, which
  reads from the PDC via `GetPrivateData` (collection-members only). No non-PDC
  query touches it.

**Verdict:** confirmed. The raw loss is never on the public ledger, never in an
event, and never returned by a non-PDC query.

---

## 5. Event payloads — none carry the raw loss

Every `SetEvent` call and its exact payload:

| Event (`SetEvent` line)            | Payload            | Fields in payload                                                                                         |
| ---------------------------------- | ------------------ | -------------------------------------------------------------------------------------------------------- |
| `ParticipantRegistered` (`:201`)   | `json.Marshal(p)`  | Participant: `objectType, mspID, role, displayName, registeredAt, rewardBalance`                          |
| `TrainingJobStarted` (`:286`)      | `json.Marshal(job)`| TrainingJob: `objectType, jobId, status, initiatorMSP, participants, labelOwnerMSP, totalEpochs, finalModelHash, createdAt, finalizedAt, rewarded` |
| `EpochLogged` (`:389`)             | `json.Marshal(record)` | EpochRecord: `objectType, jobId, epoch, `**`lossHash`**`, recordedAt`                                 |
| `TrainingFinalized` (`:431`)       | `json.Marshal(job)`| TrainingJob (as above)                                                                                    |
| `RewardsDistributed` (`:496`)      | `json.Marshal(job)`| TrainingJob (as above)                                                                                    |

- Only `EpochLogged` carries anything loss-derived, and it is the **`lossHash`
  only**.
- The `Participant`, `TrainingJob`, and `EpochRecord` structs have **no `loss`
  field**, so no event can structurally leak the raw value.
- `TestFullLifecycle` asserts the `EpochLogged` payload contains a `lossHash` key
  and **no** `loss` key.

**Verdict:** no event carries the raw loss.

---

## Summary

| # | Question              | Result                                                                 |
| - | --------------------- | ---------------------------------------------------------------------- |
| 1 | Hash determinism      | ✅ Struct marshal (stable order); same bytes hashed + PDC-stored.       |
| 2 | Float determinism     | ⚠️ Raw `float64` JSON — deterministic for endorsement; not fixed-precision (noted, unchanged). |
| 3 | Reward metric         | ✅ Uniform `+= totalEpochs` for all participants; no role weighting.    |
| 4 | Transient leakage     | ✅ Raw loss lives only in `collectionLossDetails`.                      |
| 5 | Event payloads        | ✅ No event carries the raw loss (only `EpochLogged` → `lossHash`).     |
