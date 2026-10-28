# Node retirement and recovery

Node retirement compacts stale registry metadata after a retention period. It
does not terminate a disconnected Firecracker process and does not remove node
configs, VM manifests, local volumes, shared data, or snapshots. Registry
generation fencing prevents old credentials from changing control-plane state;
it is not proof that an old process stopped.

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

The explicit empty node document and all retained volume records remain. A
local volume stays bound to its original node. Service and volume status keep
reporting an old possible VM as `previous_node_unconfirmed` until a fresh v2
empty-config observation has no tracked VM records. Retirement metadata alone
does not clear that warning.

Keep automatic retirement disabled until the configured S3/GCS provider has
passed conditional-delete validation and the deployment has exercised the
empty-config read-back, generation race, and stale-node return paths. A custom
S3-compatible endpoint must pass the runtime capability probe before the
controller will retire a record.

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
