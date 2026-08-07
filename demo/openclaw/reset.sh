#!/bin/bash
# PREFLIGHT / RESET for the rev10 demo on Cameron sr2 / node0. off-camera.
set -e
C="--context cameronbaird-sr2"; K="kubectl $C"; A=oc-node-admin0; N0=aks-nodepool1-48531884-vmss000000
echo "[reset] tear down any demo pods"
$K delete deployment fanout --ignore-not-found >/dev/null 2>&1 || true
$K delete pod agent agent-clone agent-clone2 --ignore-not-found >/dev/null 2>&1 || true
for i in $(seq 1 60); do N=$($K get pods --no-headers 2>/dev/null | grep -cE "^fanout-|^agent" || true); [ "$N" = 0 ] && break; sleep 2; done
echo "[reset] clear my snapshots (leave cameron busybox-kata* alone)"
$K exec $A -- chroot /host bash -c "for p in \$(pgrep -f \"[c]loud-hypervisor\"); do grep -q agent-v1 /proc/\$p/maps 2>/dev/null && { echo FATAL clone maps snapshot; exit 1; }; done; rm -rf /run/vc/vm/snapshots/agent-v1; echo cleared"
echo "[reset] refresh demo-tools configmap"
$K create configmap demo-tools --from-file="$HOME/demo-tools/" --dry-run=client -o yaml | $K apply -f - >/dev/null
echo "[reset] verify node0 TOML sizing (must be 4096)"
$K exec $A -- chroot /host grep -E "^default_memory" /usr/share/defaults/kata-containers/configuration-clh-preview.toml
echo "[reset] clear chatroom"
$K exec $A -- chroot /host curl -s -m 5 -X POST "http://$($K get svc fruit-stand -o jsonpath="{.spec.clusterIP}")/api/clear" >/dev/null 2>&1 || true
echo "[reset] READY. chatroom: http://$($K get svc fruit-stand -o jsonpath="{.status.loadBalancer.ingress[0].ip}")"
