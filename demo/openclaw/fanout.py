#!/usr/bin/env python3
# fanout.py — wake N agents from ONE snapshot and watch them introduce themselves.
# nobody prompts them: waking up IS the prompt (auto-fire baked into the snapshot).

import functools, json, subprocess, time, urllib.request
print = functools.partial(print, flush=True)   # live output even when piped

REPLICAS = 10                                                       # how many agents to wake (edit me)
ROOM = "http://fruits-demo-mx.northeurope.cloudapp.azure.com"       # the public chatroom

def kubectl(*args):
    return subprocess.run(["kubectl", *args], capture_output=True, text=True).stdout.strip()

def room_messages(after=0):
    with urllib.request.urlopen(f"{ROOM}/api/messages?after={after}", timeout=10) as r:
        return json.load(r)

# 1) remember where the chatroom left off, so we only show NEW arrivals
last_id = max([m["id"] for m in room_messages()] or [0])

# 2) launch: one deployment, every replica restored from the same frozen mind
t0 = time.monotonic()
kubectl("apply", "-f", "fanout.yaml")
kubectl("scale", "deployment/fanout", f"--replicas={REPLICAS}")

# 3) wait until every replica is Ready (each one is a full VM restore, not a boot)
while int(kubectl("get", "deployment/fanout", "-o", "jsonpath={.status.readyReplicas}") or 0) < REPLICAS:
    if time.monotonic() - t0 > 300:
        raise SystemExit("gave up: replicas not Ready after 300s - check the snapshot exists")
    time.sleep(0.1)
print(f"{REPLICAS}/{REPLICAS} replicas became Ready in {time.monotonic()-t0:.3f} seconds")

# 4) what each agent does on its own the moment it wakes (baked into the snapshot):
print("each clone now detects its NEW ip - that's how it knows it just woke up - and asks itself:")
print('  "In 2 sentences, say which fruit is the best and why."')
print()

# 5) stream the room as the agents check in
seen = 0
while seen < REPLICAS and time.monotonic() - t0 < 180:
    for m in room_messages(after=last_id):
        last_id = m["id"]
        seen += 1
        print(f'  [{seen:2d}/{REPLICAS}] {m["user"]}: {m["message"][:70]}')
    time.sleep(0.5)
print(f"ALL {seen} AGENTS POSTED in {time.monotonic()-t0:.3f} seconds")
