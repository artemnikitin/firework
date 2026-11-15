# Node retirement and recovery

Node retirement compacts stale registry metadata after a retention period. It
does not terminate a disconnected Firecracker process and does not remove node
configs, VM manifests, local volumes, shared data, or snapshots. Registry
generation fencing prevents old credentials from changing control-plane state;
it is not proof that an old process stopped.

## Malformed registry records

A malformed record, mismatched node ID, negative generation, or unsupported
lifecycle state is excluded from scheduling and retirement. Its node config
and registry object remain untouched. Read-only visibility reports the source
key through the existing bounded `problems` list. No durable node-quarantine
objects or quarantine API fields are created.

## Automatic retirement

The control-plane defaults keep retirement disabled and dry-run enabled:

```yaml
node_down_retention: 24h
node_stale_retention: 168h
node_retirement_enabled: false
node_retirement_dry_run: true
```

Both retention periods must exceed `node_stale_ttl`. Explicit shutdown time is
measured from the down transition; unexpected loss is measured from the last
heartbeat. Before deleting a registry record, the controller verifies that the
current rendered node document and its immutable copy both contain an
explicit `services: []` config with matching revision metadata. It then
conditionally marks the exact registry generation, writes a durable
generation tombstone and its compact index, and conditionally deletes only
that registry version. A newer registration wins the compare-and-swap race.

The explicit empty node document remains authoritative at its own immutable
revision even after the fleet publishes later revisions. All retained volume
records remain, and a local volume stays bound to its original node.

`previous_node_unconfirmed` clears per service when the old node reports a
fresh, complete v2 inventory at its applied current node config, which no
longer assigns or reports that service. Other services may remain on that
node. The waiting period also ends when the node is retired or reaches its
configured down/stale retention threshold, even with retirement disabled.
Down retention uses the down-transition timestamp (with the documented legacy
fallbacks); stale retention uses the last heartbeat receipt time. Expiry ends
the control plane's wait, without proving VM termination or authorizing storage
reuse. Removed services retain unresolved ownership history until one of these
conditions occurs, without counting as desired services.

Keep automatic retirement disabled until the configured S3/GCS provider has
passed conditional-delete validation and the deployment has exercised the
empty-config read-back, generation race, and stale-node return paths. A custom
S3-compatible endpoint must pass the runtime capability probe before the
controller will retire a record.

## Placement publication pause and repair

The controller pauses placement and rendered-config publication when it
cannot read the existing placement pointer or its target revision. This
applies with retirement disabled as well. It preserves the published
placement, node configs, and ownership history, so a read failure cannot be
mistaken for an empty prior placement. Retirement maintenance is a separate
step and still runs under its configured switches.

Controller logs identify the pause with `not publishing a replacement`,
including `placement pointer targets missing state` for a dangling pointer.
Every reconcile pass retries, even when scheduling inputs have not changed.
A transient read failure therefore resumes automatically once reads succeed.

For a persistent pause, inspect the configured state prefix (default
`cp/v1/`) and repair the underlying access error or inconsistent objects:

1. Read `placements/current.json` and its `revision`, then verify that
   `placements/revisions/REVISION.json` exists, is readable JSON, and is the
   corresponding complete placement, including previous-owner history.
2. Restore an unreadable or missing object from its known-good stored version
   or backup. If the pointer itself is damaged, restore the pointer to the
   last successfully published immutable placement revision. Preserve its
   revision and ownership data; an arbitrary older revision may lose history.
3. Check the next controller pass for a successful publication and verify
   the desired, placement, and rendered revisions with `fireworkctl status`.

Deleting the pointer makes the state look like a first deployment and discards
the evidence this pause protects. Repair the pointer/revision pair instead.
The read-only status API does not perform this repair.

## Agent rollout and owner history limit

Upgrade the control plane before the agents so it can accept v2 telemetry.
A v1 agent remains accepted, including its recovery status, but cannot prove
per-service absence. Any removal or move from a v1 node can leave
`previous_node_unconfirmed` and a degraded fleet, even if the node is healthy
and never disconnected. Its continued heartbeats prevent stale expiry, so
upgrade that agent to v2 to resolve the wait with a complete observation.
Retirement expiry remains available only when its lifecycle conditions apply.

Placement history is capped at 1024 unresolved service/node owner pairs.
One desired-state change removing more than 1024 placed services cannot be
published: the controller logs `unconfirmed previous owner count exceeds
limit 1024` and keeps the previous placement and node configs. The cap is
cumulative while v1 agents keep heartbeating; for example, two removals of
600 distinct services can exceed it even when each batch is below the limit.
Smaller batches help only when v2 agents acknowledge complete absence and a
controller pass removes the resolved entries between batches. Upgrade the
agents before substantial removal or movement, and verify the ownership wait
has cleared before proceeding. Raising or redesigning this cap is a follow-up.

## Clearing a tombstone after external fencing

Tombstones fence a stable node ID indefinitely. A recovery is a deliberate
operator action and is safe only after all these steps are complete:

1. Externally power off or otherwise fence the old host so its VMs cannot
   resume, and revoke or rotate its node bootstrap credential when applicable.
2. Check the host's retained local storage pool. A reused node ID must mount
   the same retained pool that owns its local volumes; never pair it with an
   empty replacement pool. If no local volume records use the node, record
   `none` after verifying that fact.
3. Use `fireworkctl nodes --state retired` and `fireworkctl node NODE_ID` to
   read the tombstone's exact maximum generation. Verify that the current
   rendered node config is still explicit and empty.
4. Run the dedicated maintenance command with an audit reason and references
   for the fence and local-pool verification:

   ```bash
   make build-node-maintenance
   bin/firework-node-maintenance clear-node-tombstone \
     --config /path/to/node-maintenance.yaml \
     --node NODE_ID \
     --expected-generation GENERATION \
     --reason 'host externally fenced and retained pool verified' \
     --external-fence-reference 'incident-1234' \
     --retained-pool-reference 'disk-serial-or-none' \
     --confirm-host-fenced
   ```

The command refuses an active registry record, a non-empty or missing current
node config, an invalid/missing tombstone index, and a generation mismatch. It
writes a bounded audit intent before conditionally deleting the exact
tombstone token, then compare-and-swaps the index entry. If it stops after the
tombstone delete, rerun it with the same generation and attestations to finish
the index update. Audits record the operator reason, external fence reference,
local-pool reference, cleared generation, outcome, and a hash of the deleted
provider token; the provider token itself is never stored.

The command does not delete or rewrite volume records or host data. After a
successful recovery, restart the intended agent and verify its new
registration, service placement, and each volume with `fireworkctl volumes`
and `fireworkctl volume SERVICE VOLUME`. Recovery does not prove that a local
pool is mounted; the operator remains responsible for restoring the retained
pool before reusing the node ID.
