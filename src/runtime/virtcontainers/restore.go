// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/persist"
	persistapi "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/persist/api"
	vcAnnotations "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/annotations"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/compatoci"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/types"
)

// restore.go builds a real, kata-managed *Sandbox from a `kata-runtime snapshot` dir. it
// boots via NewVMFromSnapshot and skips container creation (the containers are already live
// in the snapshotted guest RAM).

// RestoreOpts parameterizes a restore.
type RestoreOpts struct {
	// id for the restored sandbox; empty -> generated. ALWAYS distinct from the source's
	// id -- the original sandbox may still be running.
	SandboxID string

	// node-authoritative hypervisor binary paths; when set they override the snapshot's
	// persisted paths (which may belong to the source node). the shim passes bare paths,
	// not its RuntimeConfig, because virtcontainers cannot import pkg/oci (import cycle).
	// empty keeps whatever the persisted config carried.
	HypervisorPath string
	KernelPath     string
	ImagePath      string

	// path to the pod's CNI network namespace (e.g. /var/run/netns/cni-<id>), read by the
	// shim from the LIVE OCI spec (containerd ran CNI ADD during RunPodSandbox). when set, the
	// restore boots the VM INSIDE this netns and adopts its tap, so the restored guest lands on
	// the real pod IP instead of the frozen snapshot IP. empty -> legacy no-netns behavior.
	// NOT sourced from the persisted config -- that carries the SOURCE sandbox's netns.
	NetNSPath string
}

// RestoreSandbox restores a snapshot directory as a fully-wired, running *Sandbox.
func RestoreSandbox(ctx context.Context, snapshotDir string, opts RestoreOpts) (_ *Sandbox, err error) {
	if _, statErr := os.Stat(filepath.Join(snapshotDir, "config.json")); statErr != nil {
		return nil, fmt.Errorf("not a snapshot dir (no config.json): %s", snapshotDir)
	}

	// fail BEFORE mutating any restore-side state (seedPersist below writes the store) if the
	// snapshot is not cloud-hypervisor: a snapshot is a CLH memory-ranges dump + CLH config.json
	// and is unrestorable under any other hypervisor. hypervisor-agnostic file-backed restore is
	// future work; here a foreign snapshot is a fatal restore error, not a silent mis-boot.
	hvType, err := snapshotHypervisorType(snapshotDir)
	if err != nil {
		return nil, err
	}
	if HypervisorType(hvType) != ClhHypervisor {
		return nil, fmt.Errorf("kata restore failed: snapshot hypervisor %q is not %q; only cloud-hypervisor restore is supported", hvType, ClhHypervisor)
	}

	// new id, always: the source sandbox may be live, so we never reuse its id.
	newID := opts.SandboxID
	if newID == "" {
		newID = genCloneID("")
	}
	if err := validateSandboxID(newID); err != nil {
		return nil, err
	}

	// seed the persist store for newID so createSandbox -> s.Restore() rehydrates
	// endpoints/devices/containers and the "state != empty" early-return fires (skipping
	// fresh-boot agent work). returns the ORIGINAL sandbox id (the guest still knows the pause
	// container by it) so adoptPauseContainer can set the pause's guestID for agent RPCs.
	origSandboxID, err := seedPersist(snapshotDir, newID)
	if err != nil {
		return nil, fmt.Errorf("seed persist for restore: %w", err)
	}

	// reuse the existing persist->live converter to rebuild the SandboxConfig under newID.
	sandboxConfig, err := loadSandboxConfig(newID)
	if err != nil {
		return nil, fmt.Errorf("load restored sandbox config: %w", err)
	}
	sandboxConfig.ID = newID

	// the snapshot's persisted config carries the right hypervisor TYPE + tuning, but its
	// on-disk binary paths may belong to the source node; prefer the live node's.
	if opts.HypervisorPath != "" {
		sandboxConfig.HypervisorConfig.HypervisorPath = opts.HypervisorPath
	}
	if opts.KernelPath != "" {
		sandboxConfig.HypervisorConfig.KernelPath = opts.KernelPath
	}
	if opts.ImagePath != "" {
		sandboxConfig.HypervisorConfig.ImagePath = opts.ImagePath
	}

	// map the rebuilt config's guest memory MAP_PRIVATE (COW). CLH-specific: the snapshot is a
	// CLH memory-ranges dump + config.json. The file-backed mapping is set here; the config.json
	// memory-private + vsock + net_fds-marker patch is applied ONLY to H2's private copy inside
	// prepareRestoreFiles (preparePrivateRestoreConfig), so the caller-owned source snapshot dir is
	// never modified (B5). Gate on CLH so a non-CLH restore does not mmap a foreign CLH RAM dump.
	if sandboxConfig.HypervisorType == ClhHypervisor {
		sandboxConfig.HypervisorConfig.FileBackedMemory = &FileBackedMemoryConfig{
			Path:   filepath.Join(snapshotDir, "memory-ranges"),
			Shared: false,
		}
	}

	// set the VM/run store paths on the hypervisor config. newSandbox normally does this
	// (sandbox.go), but createSandbox's rehydrate early-return skips it -- and without
	// VMStorePath the CLH socket paths become relative (uuid/clh-api.sock) so CLH creates them
	// under cwd instead of /run/vc/vm/<uuid>/, leaving the agent's absolute vsock unreachable.
	// fatal on error (S2): a nil/failed driver silently reintroduces the relative-socket bug,
	// surfacing only as an opaque agent-dial timeout. sandbox.go treats the same call as fatal.
	drv, derr := persist.GetDriver()
	if derr != nil {
		return nil, fmt.Errorf("restore: get persist driver for store paths: %w", derr)
	}
	if drv == nil {
		return nil, fmt.Errorf("restore: nil persist driver for store paths")
	}
	sandboxConfig.HypervisorConfig.VMStorePath = drv.RunVMStoragePath()
	sandboxConfig.HypervisorConfig.RunStorePath = drv.RunStoragePath()

	// point the network config at the pod's CNI netns so the restored VM boots INSIDE it
	// and adopts the CNI tap (real pod IP), instead of the empty netns NewVMFromSnapshot builds.
	// NetworkID IS literally the netns path (oci/utils.go). the shim sourced this from the LIVE
	// OCI spec; the persisted config's NetworkID is the SOURCE sandbox's netns and must NOT be
	// used. NetworkCreated=false: we ADOPT the CNI-created netns, we do not create/own it.
	if opts.NetNSPath != "" {
		sandboxConfig.NetworkConfig.NetworkID = opts.NetNSPath
		sandboxConfig.NetworkConfig.NetworkCreated = false
	}

	// build the Sandbox shell. with persist seeded, createSandbox early-returns a
	// rehydrated struct without doing fresh-boot agent work.
	s, err := createSandbox(ctx, *sandboxConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("create restored sandbox: %w", err)
	}
	defer func() {
		if err != nil {
			// a restored sandbox rehydrates state="running", but Sandbox.Delete
			// hard-returns for anything not Ready/Paused/Stopped -- so a plain
			// s.Delete(ctx) here is a NO-OP and leaks the cgroup slice, seeded persist
			// store, sandbox dir, and assignSandbox symlinks on any late error. force
			// state->Stopped first so resourceControllerDelete + store.Destroy actually
			// run. the stopVM defer below reaps the CLH VM; this reaps host artifacts.
			s.state.State = types.StateStopped
			s.Delete(ctx)
		}
	}()

	// the restore path depends on createSandbox's rehydrate early-return firing, which
	// requires the seeded persist to carry state="running" (the snapshot was taken of a
	// running sandbox). if it does not, createSandbox fell through to fresh-boot work OR the
	// seed was malformed -- either way the shim (T2.1) will unconditionally skip Start and we
	// would report a Running sandbox with no resumed VM. fail loudly instead.
	if s.state.State != types.StateRunning {
		err = fmt.Errorf("restored sandbox in unexpected state %q (want running); snapshot persist may be malformed", s.state.State)
		return nil, err
	}

	// reset s.network to the CLONE's CNI netns. newSandbox built s.network from the
	// config, but createSandbox -> s.Restore() -> loadNetwork OVERWROTE it with the SOURCE
	// sandbox's persisted netns + stale endpoints. rebuild it from the clone NetworkConfig
	// (NetworkID = the CNI netns path set above) so AddEndpoints/Run below operate on the RIGHT
	// namespace. only when we actually have a CNI netns to adopt.
	if opts.NetNSPath != "" {
		net, nerr := NewNetwork(&sandboxConfig.NetworkConfig)
		if nerr != nil {
			err = fmt.Errorf("restore: build clone network for netns %q: %w", opts.NetNSPath, nerr)
			return nil, err
		}
		s.network = net
	}

	// host cgroups for the sandbox (incl. the hypervisor process).
	if err = s.setupResourceController(); err != nil {
		return nil, err
	}

	// createSandbox early-returns on the rehydrated restore path and skips fsShare.Prepare,
	// so the sandbox dir assignSandbox symlinks into is never made. create just that parent
	// dir -- not full Prepare, whose slave bind-mount assignSandbox never uses and which would
	// leak on a failed restore (s.Delete early-returns on state="running" before Cleanup).
	if err = os.MkdirAll(getSandboxPath(s.id), DirMode); err != nil {
		return nil, fmt.Errorf("create restored sandbox dir: %w", err)
	}

	// coldplug the CNI netns endpoints BEFORE launching the VM (mirrors startVM's
	// createNetwork -> AddEndpoints(...,false) at sandbox.go:1027). this scans the clone netns,
	// builds the veth/tap endpoint, populates TapInterface.VMFds, and clh.addNet stashes the tap
	// fds keyed by MAC -- so the VM launch below can bind the guest NIC to the real CNI tap.
	//
	// restoreNetFence=true makes the endpoint attach PREPARE the tap DOWN with no TC redirects (the
	// fence). The stale source guest identity therefore cannot reach the target network when the VM
	// resumes; FinalizeRestoreNetwork installs the redirects + brings the tap up only after the
	// guest identity has been reconciled and verified.
	if opts.NetNSPath != "" {
		s.restoreNetFence = true
		if _, nerr := s.network.AddEndpoints(ctx, s, nil, false); nerr != nil {
			err = fmt.Errorf("restore: adopt CNI netns endpoints: %w", nerr)
			return nil, err
		}
	}

	// boot the VM from the snapshot: CreateVM -> launchAndInit (virtiofsd + CLH) ->
	// RestoreVM. returns PAUSED. run it INSIDE the CNI netns via network.Run ->
	// doNetNS (LockOSThread + setns), so CLH's exec.Command fork inherits the pod netns and its
	// tap lives in the right namespace. with no netns (opts.NetNSPath==""), Run's doNetNS is a
	// no-op in the current namespace -- identical to the no-networking behavior.
	//
	// netfds rework: pass the adopted CNI endpoints so RestoreVM backs the EXISTING guest NIC with
	// their fresh host tap FDs (CLH vm.restore net_fds) instead of the old post-resume 2nd-NIC
	// hotplug. The endpoints carry the tap VMFds AddEndpoints just populated in the CNI netns.
	vmConfig := VMConfig{
		HypervisorType:      sandboxConfig.HypervisorType,
		HypervisorConfig:    sandboxConfig.HypervisorConfig,
		AgentConfig:         sandboxConfig.AgentConfig,
		RestoreNetEndpoints: s.network.Endpoints(),
	}
	var vm *VM
	err = s.network.Run(ctx, func() error {
		var e error
		vm, e = NewVMFromSnapshot(ctx, vmConfig, snapshotDir)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("restore vm from snapshot: %w", err)
	}
	// H2 cleanup ownership (WP5.1/5.2): install immediately after the VM object exists. Until
	// assignSandbox succeeds, s.hypervisor is not yet this restored VM, so s.stopVM would stop the
	// wrong (nil/H1) hypervisor and LEAK the restored VM. Stop the actual restored VM object here;
	// after assignSandbox, ownership transfers to s.stopVM which reaps s.hypervisor (== this VM).
	vmAssigned := false
	defer func() {
		if err != nil {
			if vmAssigned {
				s.stopVM(ctx)
			} else {
				vm.Stop(ctx)
			}
		}
	}()

	// attach the restored VM to the sandbox (reuses the VM's live agent + hypervisor).
	if err = vm.assignSandbox(s); err != nil {
		return nil, fmt.Errorf("assign restored vm to sandbox: %w", err)
	}
	vmAssigned = true

	// build the host-side PAUSE/sandbox Container object keyed to
	// newID and register it in s.containers, so the shim's Start -> IOStream(newID,newID) ->
	// findContainer(newID) succeeds. Without it Start fails "Could not find the container from
	// the sandbox containers list" and containerd tears the sandbox down. Pause-container only:
	// it has no rootfs/block devices (createDevices/createMounts are no-ops), and adopting the
	// APP container here would rebuild it from the SOURCE pod's bundle + hot-plug its devices
	// into the clone -- unsafe. The app container's host-side struct is adopted later, when
	// kubelet calls CreateContainer/StartContainer per-container against the clone (a separate
	// name-keyed no-op adopt step). seedPersist already re-keyed the pause ContainerConfig's
	// ID + bundle_path to newID, so loadSandboxConfig put it in s.config.Containers.
	if err = adoptPauseContainer(ctx, s, newID, origSandboxID); err != nil {
		return nil, fmt.Errorf("adopt restored pause container: %w", err)
	}

	// NETFDS RESTORE: the VM is restored PAUSED with its EXISTING guest NIC now backed by the
	// adopted CNI endpoint's fresh host tap FDs (RestoreVM sent CLH vm.restore net_fds via the
	// endpoints threaded into VMConfig above). No second NIC is hotplugged and the frozen snapshot
	// NIC is reused, not neutralized.
	//
	//   - The guest resume is deferred to the workload StartContainer path (a CLH snapshot restores
	//     paused; resuming here would run the app before containerd asks to start it).
	//   - The guest's snapshotted network identity (MAC/IP/routes) is still the SOURCE pod's; the
	//     agent-side reconciliation to the target CNI identity happens at StartContainer (stage 4).
	//   - The host-network gate (prepare-tap-without-redirects -> activate-after-verify) is the
	//     remaining stage-4 mechanism so the stale source identity never reaches the target network.
	//
	// Leaving the VM paused here exposes the next real lifecycle boundary (host-side CreateContainer
	// adoption) without running the app or exposing it on the target network.

	if err = s.Save(); err != nil {
		return nil, fmt.Errorf("save restored sandbox state: %w", err)
	}

	s.Logger().WithField("restored-sandbox", newID).Info("restore: managed sandbox up (paused, net_fds NIC backed, fence prepared TAP-down/no-redirects)")
	return s, nil
}

// FinalizeRestoreNetwork resumes the net_fds-restored VM and reconciles its guest NIC to the target
// CNI identity. It is called from the shim's workload StartContainer path (the restored sandbox VM
// is paused until then). Sequence (§7.4):
//  1. resume the VM once (a CLH snapshot restores paused; without resume neither the app nor the
//     kata-agent can execute).
//  2. list the guest interfaces to capture the CURRENT (snapshotted, source-pod) identity.
//  3. apply the target CNI identity via the agent, then read back and verify.
//
// FinalizeRestoreNetwork resumes the net_fds-restored VM and reconciles its single guest NIC to the
// exact target CNI identity, then activates the host-network fence. It is called from the workload
// StartContainer path. Sequence (WP4/WP5): resume once; select the sole guest NIC by name; drive the
// agent restore-replace (set target MAC via IFLA_ADDRESS, flush source addrs, apply target
// addrs/MTU); install target routes + neighbors; read back and verify the exact target identity
// (MAC + required addresses present + source addresses absent); then install both TC redirects and
// bring the tap up as the final exposure step; finally mark the workload Running.
//
// This is transactional and fail-only: on ANY error the fence is kept closed and the Kata-created
// tap/qdisc/filter is cleaned up, so the stale source identity is never forwarded. A failed Start is
// terminal for this target (the caller reaps H2); resume is never retried on a half-mutated VM.
func (s *Sandbox) FinalizeRestoreNetwork(ctx context.Context) (err error) {

	// abort guard (WP5): on any failure after this point, keep forwarding closed and remove the
	// Kata-created tap/qdisc/filter for the sole adopted endpoint. The tap/veth live in the pod CNI
	// netns, so run cleanup INSIDE that netns. Never leave a partially-exposed identity. Cleanup
	// errors are logged, not substituted for the initiating error.
	fenceEps := s.network.Endpoints()
	defer func() {
		if err != nil && len(fenceEps) > 0 {
			_ = s.network.Run(ctx, func() error {
				cleanupRestoreTCFence(ctx, fenceEps[0])
				return nil
			})
		}
	}()

	// 1. resume the paused restored VM (exactly once). A failed Start is terminal; do not retry.
	if err = s.hypervisor.ResumeVM(ctx); err != nil {
		return fmt.Errorf("kata restore failed: resume: %w", err)
	}

	eps := s.network.Endpoints()
	if len(eps) == 0 {
		// nothing to reconcile (no CNI netns adopted); the VM is resumed and running.
		s.Logger().Warn("restore: no adopted CNI endpoints; guest keeps its snapshot network identity")
		return nil
	}

	// 2. capture the guest's CURRENT interfaces (the snapshotted source identity). The net_fds
	// restore backs the SAME guest NIC with the fresh tap, but the guest kernel still holds the
	// source MAC/IP, so we cannot select it by the target MAC. Find the sole non-loopback NIC by
	// name and drive the agent's restore-replace path against it.
	before, lerr := s.agent.listInterfaces(ctx)
	if lerr != nil {
		return fmt.Errorf("kata restore failed: list guest interfaces: %w", lerr)
	}
	var guestNICName string
	guestNICCount := 0
	for _, gi := range before {
		if gi == nil || gi.Name == "" || gi.Name == "lo" {
			continue
		}
		guestNICCount++
		guestNICName = gi.Name
		s.Logger().WithField("guest-iface", gi.Name).WithField("mac", gi.HwAddr).
			Info("restore-netfds: guest NIC BEFORE identity replace")
	}
	if guestNICCount != 1 {
		return fmt.Errorf("kata restore failed: expected exactly one non-loopback guest NIC, found %d", guestNICCount)
	}

	ifaces, routes, neighbors, gerr := generateVCNetworkStructures(ctx, eps)
	if gerr != nil {
		return fmt.Errorf("kata restore failed: generate guest network structures: %w", gerr)
	}
	if len(ifaces) != 1 {
		return fmt.Errorf("kata restore failed: expected exactly one target CNI interface, found %d", len(ifaces))
	}

	// 3. restore-replace: install the target CNI identity onto the sole reused guest NIC. The agent
	// selects the link by NAME (the guest's current name), flushes source addresses, sets the
	// target MAC via IFLA_ADDRESS, and applies the target addrs/MTU. Networking failures are fatal.
	target := ifaces[0]
	targetName := target.Name // endpoint name, kept for route device remap
	target.Name = guestNICName
	target.Device = guestNICName
	// TCFILTER requires the GUEST NIC mac to equal the host-side CNI veth mac, because the tc
	// mirred redirect between the CNI veth and the tap does NOT translate MACs. Read the ACTUAL
	// scanned veth mac (endpoint.Properties().Iface.HardwareAddr) and give the guest THAT mac.
	// A missing/empty scanned MAC is fatal (WP4.2/B7): we must not proceed with a wrong identity.
	props := eps[0].Properties()
	if len(props.Iface.HardwareAddr) == 0 {
		return fmt.Errorf("kata restore failed: adopted endpoint has no scanned CNI veth MAC")
	}
	target.HwAddr = props.Iface.HardwareAddr.String()
	target.RawFlags |= kataIfaceRestoreReplace
	s.Logger().WithField("guest-nic", guestNICName).WithField("target-mac", target.HwAddr).
		Info("restore-netfds: applying restore-replace identity (guest mac = veth mac)")
	if _, uerr := s.agent.updateInterface(ctx, target); uerr != nil {
		return fmt.Errorf("kata restore failed: restore-replace interface %s: %w", guestNICName, uerr)
	}

	for _, r := range routes {
		if r.Device == targetName {
			r.Device = guestNICName
		}
	}
	if len(routes) > 0 {
		if _, rerr := s.agent.updateRoutes(ctx, routes); rerr != nil {
			return fmt.Errorf("kata restore failed: install target routes: %w", rerr)
		}
	}

	// install target ARP neighbors (WP4.5): stop discarding the generated set. Remap their device
	// to the guest NIC name; a failure is fatal under the fail-only contract.
	for _, n := range neighbors {
		if n != nil && n.Device == targetName {
			n.Device = guestNICName
		}
	}
	if len(neighbors) > 0 {
		if nerr := s.agent.addARPNeighbors(ctx, neighbors); nerr != nil {
			return fmt.Errorf("kata restore failed: install target neighbors: %w", nerr)
		}
	}

	// 4. read back and verify the guest NIC took the exact target identity (WP4.9): the target MAC,
	// the required target addresses, and the ABSENCE of the source addresses.
	after, aerr := s.agent.listInterfaces(ctx)
	if aerr != nil {
		return fmt.Errorf("kata restore failed: read back guest interfaces: %w", aerr)
	}
	// build the required target address set (address/prefix strings) and the source set to prove gone.
	targetAddrs := map[string]struct{}{}
	for _, a := range target.IPAddresses {
		if a != nil && a.Address != "" {
			targetAddrs[strings.ToLower(a.Address+"/"+a.Mask)] = struct{}{}
		}
	}
	sourceAddrs := map[string]struct{}{}
	for _, gi := range before {
		if gi != nil && gi.Name == guestNICName {
			for _, a := range gi.IPAddresses {
				if a != nil && a.Address != "" {
					sourceAddrs[strings.ToLower(a.Address+"/"+a.Mask)] = struct{}{}
				}
			}
		}
	}
	macOK := false
	gotAddrs := map[string]struct{}{}
	for _, gi := range after {
		if gi == nil || gi.Name != guestNICName {
			continue
		}
		s.Logger().WithField("guest-iface", gi.Name).WithField("mac", gi.HwAddr).
			Info("restore-netfds: guest NIC AFTER identity replace")
		if strings.EqualFold(gi.HwAddr, target.HwAddr) {
			macOK = true
		}
		for _, a := range gi.IPAddresses {
			if a != nil && a.Address != "" {
				gotAddrs[strings.ToLower(a.Address+"/"+a.Mask)] = struct{}{}
			}
		}
	}
	if !macOK {
		return fmt.Errorf("kata restore failed: guest NIC %s did not take target MAC %s after restore-replace", guestNICName, target.HwAddr)
	}
	// every required target address must be present.
	for want := range targetAddrs {
		if _, ok := gotAddrs[want]; !ok {
			return fmt.Errorf("kata restore failed: guest NIC %s missing target address %s after restore-replace", guestNICName, want)
		}
	}
	// no source address may remain (unless it was also a target address).
	for src := range sourceAddrs {
		if _, isTarget := targetAddrs[src]; isTarget {
			continue
		}
		if _, ok := gotAddrs[src]; ok {
			return fmt.Errorf("kata restore failed: stale source address %s still present on guest NIC %s", src, guestNICName)
		}
	}

	// identity verified: NOW activate the host-network fence (install both TC redirects, then bring
	// the tap up as the final exposure step). Until this point the tap was down with no redirects so
	// the stale source identity could never reach the target network. The tap + veth live inside the
	// pod's CNI netns, so run the activation INSIDE that netns (s.network.Run -> doNetNS), matching
	// how prepareRestoreTCFence ran during AddEndpoints.
	if ferr := s.network.Run(ctx, func() error {
		return activateRestoreTCFence(ctx, eps[0])
	}); ferr != nil {
		return fmt.Errorf("kata restore failed: activate network fence: %w", ferr)
	}

	// forwarding is live and identity verified: transition the sole adopted workload container from
	// Ready to Running (M3). It was left Ready at CreateContainer while the VM was paused. A missing
	// workload here is fatal (fail-only): we must not report start success while it stays Ready.
	idx, _, ierr := soleGuestWorkloadIndex(s)
	if ierr != nil {
		return fmt.Errorf("kata restore failed: locate adopted workload to mark running: %w", ierr)
	}
	c := s.containers[s.config.Containers[idx].ID]
	if c == nil {
		return fmt.Errorf("kata restore failed: adopted workload container %s not registered", s.config.Containers[idx].ID)
	}
	if serr := c.setContainerState(types.StateRunning); serr != nil {
		return fmt.Errorf("kata restore failed: mark adopted workload running: %w", serr)
	}
	if serr := s.Save(); serr != nil {
		return fmt.Errorf("kata restore failed: persist running state after activation: %w", serr)
	}
	s.Logger().WithField("guest-nic", guestNICName).WithField("target-mac", target.HwAddr).
		Info("restore-netfds: identity replace verified + forwarding activated")
	return nil
}

// kataIfaceRestoreReplace mirrors KATA_IFACE_RESTORE_REPLACE in the rust agent (netlink.rs): a
// raw_flags sentinel telling the agent to select the restored NIC by name and REPLACE its identity
// (flush source addrs + set the target MAC via IFLA_ADDRESS + apply target addrs). MUST match.
const kataIfaceRestoreReplace uint32 = 0x4000_0000

// adoptPauseContainer builds the host-side *Container for the restored sandbox's pause
// container (id == sandbox id == newID after seedPersist re-keyed it) and registers it in
// s.containers, WITHOUT creating it in the guest (it is already live from the snapshot).
// mirrors fetchContainers, but for the single pause entry only. the shim's
// Start/IOStream path depends on this adopt.
func adoptPauseContainer(ctx context.Context, s *Sandbox, newID, origSandboxID string) error {
	for i := range s.config.Containers {
		cc := &s.config.Containers[i]
		if cc.ID != newID {
			continue
		}
		// pull the OCI spec from the clone's containerd bundle (bundle_path annotation was
		// re-pointed to /run/containerd/.../<newID> by seedPersist).
		spec, err := compatoci.GetContainerSpec(cc.Annotations)
		if err != nil {
			return fmt.Errorf("get pause container spec: %w", err)
		}
		cc.CustomSpec = &spec
		// host-only adoption for the pause container too (HANDOFF D2): the pause container is
		// already live in the restored guest, so use the adoption constructor which skips
		// createMounts/createDevices/guest-create. newContainer would risk those side effects when
		// the per-container persist state map is empty.
		c, err := newAdoptedContainer(ctx, s, cc)
		if err != nil {
			return fmt.Errorf("new pause container: %w", err)
		}
		// c.id == newID for host-side lookups; the GUEST still knows the pause container by the
		// ORIGINAL sandbox id, so agent RPCs (signal/stop/wait on teardown) must use guestID.
		// the pause init exec-id == guest container id, so the process token is origSandboxID.
		if origSandboxID != "" {
			c.guestID = origSandboxID
			c.process = Process{Token: origSandboxID, Pid: -1}
		}
		if err := s.addContainer(c); err != nil {
			return fmt.Errorf("add pause container: %w", err)
		}
		// the pause container is already live in the restored guest, but newContainer built it
		// with empty state (no per-container persist for newID). mark it Running so the shim's
		// IOStream/signal path (which gates on Ready|Running) accepts it.
		if err := c.setContainerState(types.StateRunning); err != nil {
			return fmt.Errorf("set pause container running: %w", err)
		}
		return nil
	}
	return fmt.Errorf("pause container config (id=%s) not found in restored sandbox config", newID)
}

// RestoreContainer adopts an APP container that is ALREADY LIVE in the restored guest into the
// host-side sandbox, WITHOUT creating it in the guest. It is a HOST-ONLY adoption: it replaces the
// sole persisted workload config with the incoming target config, builds the Container via an
// adoption-only constructor (no createMounts/createDevices, no guest CreateContainer RPC -- the
// container's processes are already running from the snapshot), and marks it Ready (NOT Running)
// while the VM is still paused. It is marked Running only later, after a successful StartContainer.
//
// First-slice contract (WP3/§7.3): exactly ONE workload container. The incoming containerd workload
// is correlated POSITIONALLY to the sole persisted guest workload; its OCI spec, image, command,
// mounts, and credentials are NOT compared. The restore caller guarantees workload compatibility;
// Kata only adopts the sole persisted guest container. A hook-bearing workload is rejected.
func (s *Sandbox) RestoreContainer(ctx context.Context, contConfig ContainerConfig) (VCContainer, error) {
	// reject hook-bearing workloads for the first slice (before any host mutation).
	if spec := contConfig.CustomSpec; spec != nil && spec.Hooks != nil {
		h := spec.Hooks
		if len(h.Prestart) > 0 || len(h.CreateRuntime) > 0 || len(h.CreateContainer) > 0 ||
			len(h.StartContainer) > 0 || len(h.Poststart) > 0 || len(h.Poststop) > 0 {
			return nil, fmt.Errorf("kata restore failed: workload declares OCI hooks, which are not supported by the first restore slice")
		}
	}

	// locate the EXACTLY-one persisted workload config and its ORIGINAL guest-known id. The guest
	// keys this container by its snapshot id, not the fresh containerd id in contConfig.ID, so
	// agent-facing ops (waitProcess, signal) must target guestID. No fallback to the clone id.
	idx, guestID, err := soleGuestWorkloadIndex(s)
	if err != nil {
		return nil, err
	}
	if guestID == "" {
		return nil, fmt.Errorf("kata restore failed: persisted workload has an empty guest id")
	}

	// reject a SECOND workload adoption (HANDOFF D3): if a non-pause workload container is already
	// registered, a repeat Create must not replace the mapping again or leave two host containers.
	for id, c := range s.containers {
		if c == nil || id == s.id {
			continue
		}
		if c.config == nil || c.config.Annotations[criContainerTypeAnnotation] != criSandboxType {
			return nil, fmt.Errorf("kata restore failed: a workload container is already adopted; refusing a second adoption")
		}
	}

	// REPLACE the saved workload config in place with the incoming target config (M2): do not keep
	// both the source and the target copies. Preserve the original entry for rollback on error.
	savedCopy := s.config.Containers[idx]
	s.config.Containers[idx] = contConfig
	rollback := true
	defer func() {
		if rollback {
			s.config.Containers[idx] = savedCopy
		}
	}()

	// adoption-only constructor: common in-memory bookkeeping, NO createMounts/createDevices and NO
	// guest CreateContainer. The container is already live in the restored guest.
	c, err := newAdoptedContainer(ctx, s, &s.config.Containers[idx])
	if err != nil {
		return nil, err
	}
	// c.id == contConfig.ID (the containerd/clone id) for host-side lookups (findContainer,
	// IOStream). c.guestID == the original id for agent RPCs. the guest keys the init process by
	// exec-id == guest container id, so the process token is guestID.
	c.guestID = guestID
	c.process = Process{Token: guestID, Pid: -1}
	if err = s.addContainer(c); err != nil {
		return nil, err
	}
	// Ready, NOT Running: the VM is still paused; the container is marked Running only after a
	// successful StartContainer resumes and verifies the VM (M3). setContainerState persists the
	// sandbox (which now carries the adopted target-ID -> guest-ID mapping via the container's
	// guestID/Process.Token), so no separate Save is needed here (WP3.11).
	if err = c.setContainerState(types.StateReady); err != nil {
		// undo the addContainer on failure so we never leave a half-adopted container.
		delete(s.containers, c.id)
		return nil, fmt.Errorf("kata restore failed: persist adopted container mapping: %w", err)
	}
	rollback = false
	return c, nil
}

// newAdoptedContainer builds the host-side *Container for a restored workload WITHOUT any guest or
// device side effects. It mirrors the in-memory bookkeeping of newContainer (id/guestID/sandbox/
// rootfs/config/paths/mounts) but deliberately SKIPS createMounts, createDevices, and c.Restore():
// the container's rootfs and processes are already live in the restored guest, so mounting or
// device-plugging on the host would be wrong, and there is no per-container persist to restore yet
// (RestoreContainer writes it). Swappiness/swap annotations are honored to match newContainer.
func newAdoptedContainer(ctx context.Context, sandbox *Sandbox, contConfig *ContainerConfig) (*Container, error) {
	if !contConfig.valid() {
		return nil, fmt.Errorf("kata restore failed: invalid adopted container configuration")
	}
	c := &Container{
		id:            contConfig.ID,
		guestID:       contConfig.ID,
		sandboxID:     sandbox.id,
		rootFs:        contConfig.RootFs,
		config:        contConfig,
		sandbox:       sandbox,
		containerPath: filepath.Join(sandbox.id, contConfig.ID),
		rootfsSuffix:  "rootfs",
		state:         types.ContainerState{},
		process:       Process{},
		mounts:        contConfig.Mounts,
		ctx:           sandbox.ctx,
	}
	if resourceSwappinessStr, ok := c.config.Annotations[vcAnnotations.ContainerResourcesSwappiness]; ok {
		resourceSwappiness, perr := strconv.ParseUint(resourceSwappinessStr, 0, 64)
		if perr == nil && resourceSwappiness > 200 {
			perr = fmt.Errorf("swappiness should not be bigger than 200")
		}
		if perr != nil {
			return nil, fmt.Errorf("kata restore failed: invalid swappiness annotation: %w", perr)
		}
		if c.config.Resources.Memory == nil {
			c.initConfigResourcesMemory()
		}
		c.config.Resources.Memory.Swappiness = &resourceSwappiness
	}
	if resourceSwapInBytesStr, ok := c.config.Annotations[vcAnnotations.ContainerResourcesSwapInBytes]; ok {
		v, perr := strconv.ParseUint(resourceSwapInBytesStr, 0, 64)
		if perr != nil {
			return nil, fmt.Errorf("kata restore failed: invalid swap-in-bytes annotation: %w", perr)
		}
		if c.config.Resources.Memory == nil {
			c.initConfigResourcesMemory()
		}
		swap := int64(v)
		c.config.Resources.Memory.Swap = &swap
	}
	return c, nil
}

// soleGuestWorkloadIndex returns the index in s.config.Containers of the single persisted workload
// container and its ORIGINAL guest-known id, erroring unless there is EXACTLY one non-pause,
// non-sandbox workload entry. There is NO fallback to the clone id.
func soleGuestWorkloadIndex(s *Sandbox) (int, string, error) {
	idx := -1
	count := 0
	for i := range s.config.Containers {
		cc := &s.config.Containers[i]
		// skip the pause/sandbox container (already re-keyed to the sandbox id == s.id)
		if cc.ID == s.id {
			continue
		}
		if cc.Annotations[criContainerTypeAnnotation] == criSandboxType {
			continue
		}
		count++
		idx = i
	}
	switch count {
	case 1:
		return idx, s.config.Containers[idx].ID, nil
	case 0:
		return -1, "", fmt.Errorf("kata restore failed: no persisted workload container found in snapshot (expected exactly one)")
	default:
		return -1, "", fmt.Errorf("kata restore failed: snapshot has %d workload containers; the first restore slice supports exactly one", count)
	}
}

// snapshotHypervisorType reads the persisted hypervisor type from the snapshot's persist.json
// WITHOUT mutating any restore-side state, so a non-CLH snapshot can be rejected before
// seedPersist writes the store. mirrors the field loadSandboxConfig later maps into
// SandboxConfig.HypervisorType (persist SandboxConfig.HypervisorType).
func snapshotHypervisorType(snapshotDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(snapshotDir, "persist.json"))
	if err != nil {
		return "", fmt.Errorf("read snapshot persist.json: %w", err)
	}
	var ss persistapi.SandboxState
	if err := json.Unmarshal(raw, &ss); err != nil {
		return "", fmt.Errorf("decode snapshot persist.json: %w", err)
	}
	return ss.Config.HypervisorType, nil
}

// seedPersist loads the snapshot's persist.json, rewrites the sandbox identity to newID,
// and writes it into the persist store under newID. per-application container ids are
// LEFT ALONE: the in-guest agent knows them by their original id, so rewriting would
// desync host<->guest.
func seedPersist(snapshotDir, newID string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(snapshotDir, "persist.json"))
	if err != nil {
		return "", fmt.Errorf("read snapshot persist.json: %w", err)
	}
	var ss persistapi.SandboxState
	if err := json.Unmarshal(raw, &ss); err != nil {
		return "", fmt.Errorf("decode snapshot persist.json: %w", err)
	}

	// the guest kata-agent still knows the pause container by the ORIGINAL sandbox id (== the
	// original SandboxContainer); capture it before we overwrite, so the caller can set the
	// pause container's guestID for agent RPCs.
	origSandboxID := ss.SandboxContainer

	// the reused kata-agent dials AgentState.URL (loaded by s.Restore() -> loadState). the
	// snapshot carries the original id; assignSandbox symlinks /run/vc/vm/<newID> to the
	// restored VM dir, so rebuild the url from newID (a substring replace would over-match a
	// short --name like "vm" or "1024" and corrupt the path/port). without this the agent
	// talks to the source sandbox and the restore is immediately torn down.
	if ss.AgentState.URL != "" {
		ss.AgentState.URL = fmt.Sprintf("hvsock:///run/vc/vm/%s/clh.sock:1024", newID)
	}
	// the on-disk sandbox dir is keyed by ss.SandboxContainer (persist/fs.ToDisk), and
	// SandboxConfig has no ID field, so this is the only identity rewrite needed.
	ss.SandboxContainer = newID
	// cgroup paths embed the old id; clear them so setupResourceController re-derives
	// fresh ones rather than colliding with the source sandbox's cgroups.
	ss.SandboxCgroupPath = ""
	ss.OverheadCgroupPath = ""
	ss.CgroupPaths = nil
	// the old clh pid + sockets are dead in the restoring process; assignSandbox swaps in the
	// freshly-launched hypervisor, but zero them so a failure BEFORE that can not signal a
	// stale/recycled pid or deref a nil client during deferred cleanup.
	ss.HypervisorState.Pid = 0
	ss.HypervisorState.VirtiofsDaemonPid = 0
	ss.HypervisorState.APISocket = ""

	// rehydrate identity of the PAUSE/sandbox container so the restored Sandbox gets a
	// host-side Container object keyed to newID. the shim's Start ->
	// IOStream(newID, newID) -> Sandbox.findContainer(newID) needs it; without it Start fails
	// "Could not find the container from the sandbox containers list" and containerd tears the
	// sandbox down. the sandbox/pause container's id == the sandbox id, so it must move to
	// newID (like ss.SandboxContainer). its bundle_path + sandbox-id annotations must point at
	// the CLONE's containerd bundle (containerd created /run/containerd/.../<newID>/ for us).
	// APP containers are LEFT ALONE: the in-guest agent knows them by their original id, and
	// their bundles belong to the source pod -- rewriting would desync host<->guest.
	for i := range ss.Config.ContainerConfigs {
		cc := &ss.Config.ContainerConfigs[i]
		if cc.Annotations[criContainerTypeAnnotation] == criSandboxType {
			cc.ID = newID
			if cc.Annotations == nil {
				cc.Annotations = map[string]string{}
			}
			cc.Annotations[ociBundlePathAnnotation] = filepath.Join(containerdBundleBase, newID)
			cc.Annotations[criSandboxIDAnnotation] = newID
		}
	}

	store, err := persist.GetDriver()
	if err != nil {
		return "", err
	}
	// container map stays empty on disk: per-container STATE is restored from guest RAM, not
	// host persist. the host-side Container STRUCTS are rebuilt post-createSandbox via
	// s.fetchContainers (which tolerates missing per-container persist).
	if err := store.ToDisk(ss, map[string]persistapi.ContainerState{}); err != nil {
		return "", err
	}
	return origSandboxID, nil
}

// CRI + OCI annotation keys used to identify + re-key the pause container on restore.
const (
	criContainerTypeAnnotation = "io.kubernetes.cri.container-type"
	criSandboxIDAnnotation     = "io.kubernetes.cri.sandbox-id"
	criSandboxType             = "sandbox"
	ociBundlePathAnnotation    = "io.katacontainers.pkg.oci.bundle_path"
	containerdBundleBase       = "/run/containerd/io.containerd.runtime.v2.task/k8s.io"
)

// genCloneID returns name if given, else a random clone-<hex> id.
func genCloneID(name string) string {
	if name != "" {
		return name
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "clone-fallback"
	}
	return "clone-" + hex.EncodeToString(b)
}

// validateSandboxID rejects ids that would let on-disk state dirs escape their base
// (no path separators or ".."). duplicated here because virtcontainers cannot import
// katautils (import cycle).
func validateSandboxID(id string) error {
	if id == "" {
		return fmt.Errorf("sandbox id is required")
	}
	if id == "." || id == ".." || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid sandbox id %q: must not contain path separators or ..", id)
	}
	return nil
}
