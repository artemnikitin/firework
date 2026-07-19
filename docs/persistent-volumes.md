# Persistent Volumes

Firework keeps application data separate from the writable root filesystem by
attaching one quota-sized ext4 image per declared volume.

## Application configuration

```yaml
volumes:
  - name: data
    type: local
    mount_path: /var/lib/application
    size: 20Gi
```

`size` is optional. Resolution uses the explicit value, then the matching
`volume_defaults.local_size` or `volume_defaults.shared_size`, then `10Gi`.
The resolved quota is a hard filesystem maximum; ext4 metadata makes usable
space slightly smaller. Only positive integer `Mi` and `Gi` values are
accepted. A service may declare at most 25 volumes (`/dev/vdb` through
`/dev/vdz`), and Firework rejects a generated guest payload that would exceed
the portable kernel command-line limit.

`local` volumes are bound permanently to the node where their durable record
was created. If that node is unavailable, the service remains pending instead
of receiving an empty volume elsewhere. Removing a service retains its record
and data.

The control plane fills `bound_node` in rendered node config from its durable
record. In direct Git mode, an operator must add the physical node's stable
identity to `bound_node` in the resolved node config; the agent rejects a
missing or mismatched binding rather than treating a reusable node label as
storage identity.

`shared` is part of the provider-neutral schema and deployment contract, but
runtime placement remains pending until the EFS/Filestore partition-fencing
gates in [issue #32](https://github.com/artemnikitin/firework/issues/32) are
complete. The durable per-VM supervisor preserves ownership across agent
restarts, but that alone cannot prove exclusive access during cloud filesystem
partitions, so Firework continues to fail closed for shared volumes.

## Agent configuration

The operator provisions and mounts storage before starting the agent:

```yaml
storage:
  local:
    path: /var/lib/firework/volumes
    capacity: 500Gi
  shared:
    backend_id: primary
    path: /mnt/firework-shared
    capacity: 1Ti
```

Configured paths must be actual mount points. `capacity` is a logical admission
budget and retained volumes continue to reserve it. Firework creates sparse
ext4 images with no root-reserved percentage, but also checks host free space
before creation or growth.

## Resize and retention

Quota changes are offline. Before stopping an existing VM, Firework validates
identity, binding, capacity, filesystem health, and shrink feasibility. It then
records a durable resize transaction, grows the file before ext4, or shrinks
ext4 before truncating the file. Ambiguous states fail closed.

Deleting application YAML never deletes a volume. Permanent deletion is a
manual operator action: first stop/remove the service placement, back up the
volume if required, then remove both its host directory and
`cp/v1/volumes/<service>/<volume>.json`. Never delete only one side and expect
automatic adoption.

Service detail in the UI, API, and `fireworkctl service <name>` includes the
logical ID, binding/backend, desired and applied quota, resize generation, and
preparation state.
