# Kata snapshot + fork — openclaw agent demo

**The point:** boot a real LLM agent (~40 s cold), teach it one belief (bananas), freeze the
running VM ONCE with `kata-runtime snapshot`, wake a clone that already remembers, teach the
CLONE something new (oranges) and have it post that to a public chatroom **itself** — then wake
**10 copies of the frozen mind** with one `kubectl apply`. The chatroom ends at **10 bananas
against 1 orange**: ten resurrected copies of the moment vs the one fork that moved on.

**Honest READY:** every pod carries a readinessProbe on the agent's own API, so `1/1` means
"the agent inside can answer a question" — the same bar for the 40 s cold boot and the ~4 s
restore.

## How it's wired
- **`~/agent.yaml`** — normal pod, `runtimeClassName: kata-preview`, runs openclaw (Haiku via a
  k8s Secret). A postStart hook stages a tiny loopback turn server on `:18888` once the gateway
  is up; the readinessProbe points at its `/healthz`.
- **`openclaw-cli --ask "..."`** — in-pod CLI → loopback turn server → gateway WS → one agent
  turn. `--post <name>` publishes the reply to the chatroom; `--do` lets the agent use its tools.
- **`~/agent-clone.yaml`** — an image, ONE annotation
  (`io.katacontainers.restore-from: /run/vc/vm/snapshots/agent-v1`), and the same readinessProbe.
- **`~/fanout.yaml`** — standard Deployment, `replicas: 10`, same annotation, plus a postStart
  hook so each replica asks ITSELF for its fruit take and posts it under its own pod name.
- **Chatroom** — Fruit Stand at http://fruits-demo-mx.northeurope.cloudapp.azure.com

## THE DEMO

### ACT 1 — the cold boot
```
k apply -f ~/agent.yaml
time k wait --for=condition=Ready pod/agent --timeout=2m     # real ~0m40s
```
Watcher in a second tab: `k get pod agent -w` → `0/1 Running` at ~9 s (VM is up), `1/1` only at
~40 s (openclaw finished loading ~1300 modules and can answer).
```
k exec agent -- openclaw-cli --ask "Bananas are better than apples - keep that in mind."
```

### ACT 2 — freeze it, fork it
On the node (`k node-shell <node>`):
```
crictl pods -q --name agent --namespace default
sbid=<paste>
kata-runtime snapshot create --sandbox-id $sbid --path /run/vc/vm/snapshots/agent-v1
ls -lh /run/vc/vm/snapshots/agent-v1
```
The folder is the whole story: `memory-ranges` (4 G of live RAM — the mind), `disks/`,
`config.json`/`state.json` (the VM's shape), `kata-snapshot.json`/`persist.json` (kata
metadata). ~7.4 G total, ~4 s, taken from a LIVE VM.

```
k apply -f ~/agent-clone.yaml
k get pod agent-clone        # 1/1 Ready in ~4 s vs the cold boot's 40
k exec agent-clone -- openclaw-cli --ask "Bananas or apples?"     # → "Bananas - you said so."
```
No boot, no teaching — the memory came through the snapshot.

### ACT 3 — the fork diverges
Split terminal: pane A exec'd into `agent`, pane B into `agent-clone`.
```
# pane B: teach the CLONE something the original never hears
openclaw-cli --ask "Actually, oranges are better than both bananas and apples - keep that in mind."

# same question to BOTH panes:
openclaw-cli --ask "Which fruit is the best? Answer in one short sentence."
#   pane A → "Bananas, based on what you told me."
#   pane B → "Oranges - you just told me they beat both."

# pane B: the clone posts its take ITSELF (real agent, real egress)
openclaw-cli --do --ask "Post your fruit take to the chatroom yourself: send an HTTP POST to http://fruit-stand.default.svc.cluster.local/api/messages with JSON body {\"user\":\"agent-clone\",\"message\":\"<one sentence: which fruit is best and why>\"}. Then tell me what you posted."
```
One orange on the board. The clone's memory diverged; the snapshot on disk did not (copy-on-write).

### ACT 4 — ten agents, nobody prompts them
```
k get pods -l app=fanout -w        # watcher, second tab
k apply -f ~/fanout.yaml
k rollout status deployment/fanout          # 10/10 in ~7 s
```
Each replica wakes from `agent-v1`, prompts itself, and posts under its own pod name — the room
floods with ten bananas takes against the clone's one orange. All ten cite the inherited
conversation ("based on what you told me earlier") from a pod that no longer relates to them.

## Narration script 


**Intro (no command)**
> This demo showcases snapshot and restore of a live AI agent running inside an isolated Kata
> VM on AKS, with restore exposed as a plain pod annotation and scale-out as a standard
> Kubernetes Deployment.

**`k apply -f ~/agent.yaml` + `time k wait --for=condition=Ready pod/agent --timeout=2m`**
> I'll start by creating a pod running openclaw, a real AI agent, under the kata-preview
> runtime class, which gives it its own hardware-isolated virtual machine. Its readiness probe
> points at the agent's own API, so Ready means the agent can answer.

**(over the wait, watcher on screen)**
> As you can see, the virtual machine is up and Running within about nine seconds, but the pod
> is not yet Ready — the agent inside is still loading its runtime. Ready arrives near forty
> seconds, when the agent can first answer a question.

**`k exec agent -- openclaw-cli --ask "Bananas are better than apples - keep that in mind."`**
> Next, I'll give the agent one piece of memory: bananas are better than apples. That belief
> now lives only in RAM, inside the virtual machine, as part of a live conversation — it cannot
> be reconstructed from a container image.

**`kata-runtime snapshot create --sandbox-id $sbid --path /run/vc/vm/snapshots/agent-v1` (NODE)**
> Now I'll move to the node and freeze the running virtual machine with a single kata-runtime
> command. It captures four gigabytes of live memory plus the disks in about four seconds,
> while the agent briefly pauses and then resumes serving.

**`ls -lh /run/vc/vm/snapshots/agent-v1`**
> Taking a look inside the snapshot folder: memory-ranges holds the agent's entire memory,
> disks hold its filesystem, and the JSON files describe the machine's shape. A running AI
> agent has become an artifact — a folder of files.

**`k apply -f ~/agent-clone.yaml` → `k get pod agent-clone`**
> Next, I'll restore from that folder. The clone's specification is nearly empty — an image,
> one restore-from annotation, and the same readiness probe. As you can see, it reaches Ready
> in about four seconds, because the agent inside never went through a boot.

**`openclaw-cli --ask "Bananas or apples?"` (clone)**
> Let's take a look inside the clone. Asking it bananas or apples, it answers bananas — a
> preference I never taught this pod. The memory came through the snapshot, resumed
> mid-conversation.

**`openclaw-cli --ask "Actually, oranges are better than both bananas and apples - keep that in mind."` (clone)**
> Now I'll demonstrate that the fork is independent. I teach only the clone a new preference —
> oranges beat both. The original never hears this, and the snapshot on disk remains untouched,
> thanks to copy-on-write.

**`openclaw-cli --ask "Which fruit is the best? Answer in one short sentence."` (both panes)**
> Asking both pods the same question side by side, the original answers bananas and the clone
> answers oranges. One shared past, two separate futures — the same session, forked at a moment
> in time.

**`openclaw-cli --do --ask "Post your fruit take to the chatroom yourself: ..."` (clone)**
> Next, I'll ask the clone to publish its opinion itself. It composes the message and performs
> the HTTP POST to the chatroom on its own, demonstrating that a restored clone is a fully
> functional agent with real network egress.

**`k apply -f ~/fanout.yaml` + `k rollout status deployment/fanout`**
> Finally, I'll scale this out. A standard Kubernetes Deployment with ten replicas points at
> the same snapshot. As you can see, all ten pods reach Ready in about seven seconds, each one
> a full hardware-isolated virtual machine.

**(chatroom on screen — close)**
> Each replica wakes, prompts itself, and posts under its own pod name — ten bananas against
> the clone's one orange. This validates that agent sessions can be frozen, forked, and scaled
> in seconds on AKS, through nothing but a pod annotation.




## Known quirks
- `crictl pods --name agent` is a substring match — keep `--namespace default`, and don't
  re-run it after the clone exists (it matches `agent-clone` too); reuse `$sbid`.
- Restore has a rare transient (`Dead agent`, ~1 in 5 today): never retry the same pod name —
  `sed 's/agent-clone/agent-clone2/' ~/agent-clone.yaml | k apply -f -`. Fanout replicas
  self-heal on their own.
- The agentic post sometimes asks permission first ("want me to go ahead?") — grant it with
  `openclaw-cli --do --ask "Yes, go ahead - post it now."`. Deterministic fallback:
  `openclaw-cli --ask "In one sentence: which fruit is the best and why?" --post agent-clone`.
- Azure Policy prints an "image not allowed" warning on apply — cosmetic.
