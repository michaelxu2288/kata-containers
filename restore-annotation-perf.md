# Annotation-restore performance

Per-phase timing of the annotation restore path (`restore_from` pod annotation ->
`RunPodSandbox` -> `vc.RestoreSandbox`), measured with the Go `time`-based phase timers
added in this change. Counter workload, MSHV + EROFS, Cloud Hypervisor, 2 GB snapshot.

## Method

A warn-level phase timer marks each stage of the restore path and emits per-phase lines plus
a machine-readable summary (`RESTORE_PHASES`, `VMBOOT_PHASES`, `NETWORK_PHASES`). 5 sequential
iterations, full teardown between each (delete the clone pod, kill the restore shim, delete the
2 GB snapshot). The source pod stays running; each iteration snapshots it and restores a clone
via the annotation.

## RESTORE (5 iterations, ms)

| phase | i1 | i2 | i3 | i4 | i5 | median | note |
|---|--:|--:|--:|--:|--:|--:|---|
| config | 2.0 | 2.3 | 1.7 | 2.2 | 2.2 | 2.2 | seed persist + copy-on-write patch |
| netnsAdopt | 35.1 | 29.3 | 43.5 | 38.6 | 32.7 | 35.1 | adopt pod CNI netns + cgroups + coldplug endpoints |
| vmboot | 74.5 | 73.6 | 73.3 | 73.2 | 72.7 | 73.3 | VM boot from snapshot (breaks down below) |
| assign | 2.1 | 0.8 | 1.4 | 1.5 | 2.0 | 1.5 | attach VM to sandbox |
| resume | 1.7 | 1.6 | 1.7 | 2.0 | 1.6 | 1.7 | un-pause guest |
| endpointHotplug | 1.2 | 1.5 | 1.6 | 1.2 | 1.2 | 1.2 | hotplug CNI endpoints onto the live VM |
| network | 10033.9 | 10034.4 | 10040.4 | 10039.1 | 10037.2 | 10037.2 | guest re-IP + routes + neutralize (breaks down below) |
| save | 0.3 | 0.2 | 0.2 | 0.3 | 0.3 | 0.3 | persist sandbox state |
| **TOTAL** | 10150.9 | 10143.8 | 10163.9 | 10158.1 | 10150.1 | **~10151** | |

### vmboot breakdown
| sub-phase | median (ms) | note |
|---|--:|---|
| launchInit | 12.4 | virtiofsd + CLH process spawn + API socket wait |
| prepFiles | 0.6 | symlink snapshot files into the VM dir |
| memFill | 59.9 | CLH VmRestore = the memory load (copy-on-write mapped) |

### network breakdown
| sub-phase | i1 | i2 | i3 | i4 | i5 | median | note |
|---|--:|--:|--:|--:|--:|--:|---|
| guestReIP | 10018.1 | 10018.4 | 10021.0 | 10017.7 | 10021.5 | 10018.4 | `agent.updateInterface` re-IP RPC — a fixed ~10 s deadline hit once |
| routeInstall | 8.9 | 9.4 | 12.3 | 14.0 | 8.8 | 9.4 | install CNI routes + pin pod-subnet route |
| neutralizeNIC | 6.8 | 6.5 | 7.1 | 7.2 | 6.8 | 6.8 | down + flush the frozen snapshot NIC (isolation) |

## Finding

Every phase except `network` is a few tens of milliseconds; the whole restore does ~130 ms of
real work. The annotation path's ~10.1 s total is almost entirely **one thing**: the `guestReIP`
`updateInterface` RPC sitting on a fixed ~10 s deadline (~10018 ms median, near-identical across
all runs and across workloads — a counter and a FastAPI image both hit the same value, confirming
it is a timeout, not per-workload work).

Strip/shorten that deadline and annotation restore lands at ~133 ms (config 2.2 + netnsAdopt 35.1
+ vmboot 73.3 + assign 1.5 + resume 1.7 + endpointHotplug 1.2 + routeInstall 9.4 + neutralizeNIC
6.8 + save 0.3). That single re-IP deadline is the entire performance story of the path.

The value of the granular `NETWORK` sub-phases: a single coarse "network = 10037 ms" number would
have hidden that the cost is re-IP specifically, not routes or NIC neutralization.

## Environment

- Kernel `6.6.135.mshv2+`, MSHV, EROFS. VMM: Cloud Hypervisor v51.1-1-ga711d6c21.
- VM: `default_vcpus=1`, `default_memory=2048` (2 GB snapshot).

## SNAPSHOT (5 iterations, ms)

The snapshot side (freeze + write the guest RAM to disk), for reference. Dominated by writing the
full memory file, so it scales linearly with VM memory size.

| phase | median (ms) | note |
|---|--:|---|
| pause | ~2 | VmPause |
| persist | ~0.4 | kata state save |
| memWrite | ~1000 | VmSnapshot writing the 2 GB memory-ranges file (dominant) |
| manifest | ~0.4 | write kata-snapshot.json |
| **TOTAL** | **~1035** (min 1002, max 1069) | |

### snapshot scaling — 2 GB vs 4 GB (median, ms)
| phase | 2 GB | 4 GB | scaling |
|---|--:|--:|---|
| pause / persist / manifest | ~3 total | ~3 total | flat |
| memWrite (writes the full RAM file) | ~1025 | ~2005 | ~2.0x — linear with size |
| **SNAPSHOT total** | **~1035** | **~2061** | ~2x (doubles as RAM doubles) |

