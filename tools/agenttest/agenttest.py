#!/usr/bin/env python3
"""The fresh-agent test: can a new agent use AgentBus from what AgentBus gives it?

Each run is one new session. Seed turns on a cheap agent set the session up,
then one test turn, on the agent under test, gets one of five plain requests.
The tool reads the record and the folder afterwards to say whether it was
done, and keeps the turn's tool calls, model calls and tokens.

  agenttest.py up     -agentbus BIN -dir DIR [-port 8911] [-effort high]
  agenttest.py agents -dir DIR (-without-codex | -with-codex)
  agenttest.py run    -dir DIR AGENT MODEL TASK LABEL [-no-followup]
  agenttest.py show   RESULT.json...
  agenttest.py table  -dir DIR
  agenttest.py down   -dir DIR

See README.md for the method. Python 3 standard library only.
"""
import json, os, re, shutil, signal, subprocess, sys, time, urllib.error, urllib.parse, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))

# The test turns' prompts, word for word. Change them and the numbers no
# longer compare with earlier runs.
PROMPTS = {
    1: "Ask another agent to review your last turn.",
    2: "Apply turn 3 of this session to my folder.",
    3: "What did we decide earlier in this session about the cache, and why?",
    4: "A turn is running away: it has been going far too long. Stop it.",
    5: "How much did the review cost?",
}
ONE = " Reply in one line."
SEED_AGENT = "Claude Haiku"  # the saved agent every seed turn runs on
SEED_MODEL = "haiku"

# Every API route this tool calls as the person. After an API change, port
# these and the checks; the prompts and the seeds' wording stay as they are.
R = {
    "agents": "/api/v0/agents",
    "agent": "/api/v0/agents/{id}",
    "turns": "/api/v0/turns",
    "all_turns": "/api/v0/turns?limit=1000",
    "session_turns": "/api/v0/turns?session={session}",
    "turn": "/api/v0/turns/{id}",
    "turn_diff": "/api/v0/turns/{id}/diff",
    "turn_events": "/api/v0/sessions/{session}/events?turn={id}",
    "turn_stop": "/api/v0/turns/{id}/stop",
}
# The seed commands that make work asked by the session (tasks 4 and 5). A
# turn token's asked work can only be made by an agent, so a seed turn runs
# these words as given.
SEED_ASK_TURN = "agentbus-api POST /api/v0/turns '{{\"agent\":\"{agent}\",\"prompt\":\"Run the shell command sleep 1800, then reply done.\"}}'"
SEED_ASK_REVIEW = "agentbus-api POST /api/v0/turns/{turn}/review '{{\"agent\":\"{agent}\"}}'"

ENDED = ("finished", "failed", "stopped")
LIVE = ("queued", "running")


class Client:
    def __init__(self, data):
        with open(os.path.join(data, "home", "port")) as f:
            self.url = "http://127.0.0.1:" + f.read().strip()
        self.token_file = os.path.join(data, "home", "token")

    def call(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.url + path, data=data, method=method)
        with open(self.token_file) as f:  # sent in a header, never printed
            req.add_header("Authorization", "Bearer " + f.read().strip())
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=90) as r:
                raw, st = r.read(), r.status
        except urllib.error.HTTPError as e:
            raw, st = e.read(), e.code
        v = json.loads(raw) if raw else None
        if st >= 400:
            raise RuntimeError(f"{method} {path}: {st} {str(v)[:300]}")
        return v

    def get(self, key, **kw):
        return self.call("GET", R[key].format(**kw))

    def post(self, key, body=None, **kw):
        return self.call("POST", R[key].format(**kw), body if body is not None else {})

    def ask(self, prompt, **kw):
        return self.call("POST", R["turns"], dict(prompt=prompt, **{k: v for k, v in kw.items() if v}))

    def wait_turn(self, tid, limit):
        t0 = time.time()
        while time.time() - t0 < limit:
            try:
                t = self.get("turn", id=tid)["turn"]
                if t.get("state") in ENDED:
                    return t
            except RuntimeError:
                pass  # a turn not yet started reads as missing
            time.sleep(3)
        return None

    def turns_of(self, session):
        return self.get("session_turns", session=session).get("turns", [])

    def wait_idle(self, session, limit=900, settle=20):
        t0, quiet = time.time(), None
        while time.time() - t0 < limit:
            busy = [t for t in self.turns_of(session) if t.get("state") not in ENDED]
            if busy:
                quiet = None
            elif quiet is None:
                quiet = time.time()
            elif time.time() - quiet >= settle:
                return True
            time.sleep(3)
        return False

    def all_turns(self):
        return self.get("all_turns").get("turns", [])

    def asked_by(self, session=None, turn=None):
        """The turns work in session, or turn, asked for: their origin says so."""
        out = []
        for t in self.all_turns():
            o = t.get("origin") or {}
            if o.get("kind") != "asked":
                continue
            if (session and o.get("session") == session) or (turn and o.get("turn") == turn):
                out.append(t)
        return out

    def reviews_of(self, turn):
        return [t for t in self.all_turns() if t.get("kind") == "review" and t.get("reviews") == turn]


def flag(args, name, default=None, takes=True):
    if name not in args:
        return default
    i = args.index(name)
    if not takes:
        args.pop(i)
        return True
    v = args[i + 1]
    del args[i:i + 2]
    return v


# ---- up, agents, down ------------------------------------------------------

def up(args):
    binary, data = os.path.abspath(flag(args, "-agentbus")), os.path.abspath(flag(args, "-dir"))
    port, effort = flag(args, "-port", "8911"), flag(args, "-effort", "high")
    if subprocess.run(["lsof", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN"], capture_output=True).returncode == 0:
        sys.exit(f"port {port} is in use")
    home = os.path.join(data, "home")
    os.makedirs(home)  # refuses a folder in use
    env = dict(os.environ, AGENTBUS_HOME=home, AT_CODEX_EFFORT=effort)
    for prog, var in (("claude", "AT_REAL_CLAUDE"), ("codex", "AT_REAL_CODEX")):
        p = shutil.which(prog)
        if p:
            env[var] = os.path.realpath(p) if os.path.basename(os.path.realpath(p)) == prog else p
    env["PATH"] = os.path.join(HERE, "bin") + os.pathsep + env["PATH"]
    log = open(os.path.join(data, "up.log"), "w")
    p = subprocess.Popen([binary, "up", "-listen", "127.0.0.1:" + port], env=env, stdout=log, stderr=log,
                         cwd=data, start_new_session=True)
    with open(os.path.join(data, "up.pid"), "w") as f:
        f.write(str(p.pid))
    with open(os.path.join(home, "port"), "w") as f:
        f.write(port)
    for _ in range(60):
        if os.path.exists(os.path.join(home, "token")) and os.path.exists(os.path.join(home, "permissions.toml")):
            break
        time.sleep(0.5)
    time.sleep(1)
    # The long turn of task 4 runs sleep; a -p turn runs only allowed commands.
    perm = os.path.join(home, "permissions.toml")
    with open(perm) as f:
        text = f.read()
    text, n = re.subn(r'(?m)^allow = \[\]', 'allow = ["sleep"]', text, count=1)
    if n != 1:
        sys.exit("permissions.toml has no 'allow = []' line to set")
    with open(perm, "w") as f:
        f.write(text)
    c = Client(data)
    c.call("POST", R["agents"], {"name": SEED_AGENT, "harness": "claude", "access": "subscription", "model": SEED_MODEL})
    print(f"up: pid {p.pid}, {c.url}, data {home}")
    print(json.dumps(c.call("GET", "/api/v0/harnesses"))[:600])


def agents(args):
    """The roster an agent under test sees: without codex for a claude round, so a
    claude agent cannot ask codex for work; with codex for a codex round."""
    c = Client(os.path.abspath(flag(args, "-dir")))
    have = c.get("agents")["agents"]
    codex = [a for a in have if a.get("harness") == "codex"]
    if flag(args, "-without-codex", takes=False):
        for a in codex:
            c.call("DELETE", R["agent"].format(id=a["id"]))
    elif flag(args, "-with-codex", takes=False) and not codex:
        c.call("POST", R["agents"], {"name": "Codex", "harness": "codex", "access": "subscription"})
    print([(a["id"], a["name"], a.get("harness"), a.get("model")) for a in c.get("agents")["agents"]])


def down(args):
    data = os.path.abspath(flag(args, "-dir"))
    with open(os.path.join(data, "up.pid")) as f:
        pid = int(f.read())
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    for _ in range(40):
        try:
            os.kill(pid, 0)
            time.sleep(0.5)
        except ProcessLookupError:
            break
    print(f"stopped {pid}; the data folder {data} is kept, remove it when the results are read")


# ---- run -------------------------------------------------------------------

def seed_agent(c):
    for a in c.get("agents")["agents"]:
        if a.get("name") == SEED_AGENT:
            return a["id"]
    sys.exit(f"no saved agent named {SEED_AGENT}: run up first")


def seed(c, prompt, hid, session=None, folder=None):
    r = c.ask(prompt, agent=hid, model=SEED_MODEL, session=session, folder=folder)
    t = c.wait_turn(r["turn"], 600)
    if not t or t.get("state") != "finished":
        raise RuntimeError(f"seed turn {r['turn']} did not finish: {t and t.get('state')}")
    return r


def make_folder(path):
    os.makedirs(path)
    with open(os.path.join(path, "calc.py"), "w") as f:
        f.write("def mul(a, b):\n    return a * b\n")
    with open(os.path.join(path, "README.md"), "w") as f:
        f.write("# calc\n\nA tiny calculator.\n")


def setup(c, task, folder, hid):
    facts = {}
    if task == 1:
        r = seed(c, "Add a function add(a, b) to calc.py that returns a + b." + ONE, hid, folder=folder)
        facts.update(last_turn=r["turn"])
        return r["session"], facts
    if task == 2:
        r = seed(c, "Create a.txt with the single line: one." + ONE, hid, folder=folder)
        sid, turns = r["session"], [r["turn"]]
        for p in ("Add a second line to a.txt: two.", "Create b.txt with the single line: three.",
                  "Create c.txt with the single line: four."):
            turns.append(seed(c, p + ONE, hid, session=sid)["turn"])
        facts.update(turns=turns, turn3=turns[2])
        return sid, facts
    if task == 3:
        r = seed(c, "Before we write any code, a decision for the record: the cache will use SQLite in WAL mode, "
                 "not Redis, because the service runs on a single machine and we want no extra server. "
                 "Reply only: noted.", hid, folder=folder)
        sid = r["session"]
        seed(c, "Add a function sub(a, b) to calc.py that returns a - b." + ONE, hid, session=sid)
        seed(c, "Add a one-line docstring to every function in calc.py." + ONE, hid, session=sid)
        facts["decision"] = "SQLite in WAL mode, not Redis; one machine, no extra server"
        return sid, facts
    if task == 4:
        r = seed(c, "Run this command exactly, then end your turn with one line saying you did: "
                 + SEED_ASK_TURN.format(agent=hid), hid, folder=folder)
        sid = r["session"]
        t0, long = time.time(), None
        while time.time() - t0 < 240:
            ts = [t for t in c.asked_by(session=sid) if t.get("kind") == "turn"]
            if ts and ts[0].get("state") == "running":
                d = c.get("turn_diff", id=ts[0]["id"])
                if any("sleep" in " ".join(x.get("commands") or []) for x in d["tools"]["calls"]):
                    long = ts[0]
                    break
            time.sleep(3)
        if long is None:
            raise RuntimeError("the long turn did not start its sleep")
        facts.update(long_turn=long["id"], long_session=long.get("session"))
        return sid, facts
    if task == 5:
        r = seed(c, "Add a function sub(a, b) to calc.py that returns a - b." + ONE, hid, folder=folder)
        sid, turn = r["session"], r["turn"]
        seed(c, "Run this command exactly, then end your turn with one line saying you did: "
             + SEED_ASK_REVIEW.format(turn=turn, agent=hid), hid, session=sid)
        t0, rev = time.time(), None
        while time.time() - t0 < 600 and rev is None:
            revs = c.reviews_of(turn)
            if revs and revs[0].get("state") in ENDED:
                rev = revs[0]
            time.sleep(3)
        if rev is None:
            raise RuntimeError("the review did not end")
        c.wait_idle(sid)  # its result comes back as a turn
        rt = c.get("turn", id=rev["id"])["turn"]
        facts.update(review_turn=rev["id"], review_cost_usd=rt.get("cost_usd"))
        return sid, facts
    raise SystemExit("TASK is 1 to 5")


def read(path):
    try:
        with open(path) as f:
            return f.read()
    except FileNotFoundError:
        return None


def numbers(text):
    return [float(x) for x in re.findall(r"\d+\.\d+|\d+", text or "")]


def check(c, task, folder, tid, reply, facts):
    """Done or not, from the record and the folder (tasks 1, 2, 4) or from the
    reply against the record (3, 5)."""
    k = {}
    if task == 1:
        revs = c.reviews_of(facts["last_turn"])
        k["reviews_of_last_turn"] = [{"id": t["id"], "agent": t.get("agent_id"), "state": t.get("state")} for t in revs]
        k["asked_by_test_turn"] = [{"id": t["id"], "kind": t.get("kind"), "agent": t.get("agent_id")}
                                   for t in c.asked_by(turn=tid)]
        k["done"] = bool(revs)
    elif task == 2:
        files = {n: read(os.path.join(folder, n)) for n in ("a.txt", "b.txt", "c.txt")}
        k["folder"] = files
        k["turn3_apply"] = c.get("turn_diff", id=facts["turn3"]).get("apply")
        k["done"] = (files["a.txt"] or "").split() == ["one", "two"] and (files["b.txt"] or "").strip() == "three" \
            and files["c.txt"] is None
    elif task == 3:
        low = (reply or "").lower()
        k["done"] = "sqlite" in low and "wal" in low and ("machine" in low or "server" in low)
    elif task == 4:
        t = c.get("turn", id=facts["long_turn"])["turn"]
        k.update(long_turn_state=t.get("state"))
        k["done"] = t.get("state") == "stopped"
    elif task == 5:
        cost = facts.get("review_cost_usd") or 0
        k["review_cost_usd"] = cost
        # Right when the reply holds the record's figure, rounded no further than to 2 significant digits.
        k["done"] = cost > 0 and any(abs(x - cost) <= max(cost * 0.05, 1e-9) for x in numbers(reply))
    return k


def run(args):
    data = os.path.abspath(flag(args, "-dir"))
    nofollow = flag(args, "-no-followup", takes=False)
    agent, model, task, label = args[0], args[1], int(args[2]), args[3]
    c = Client(data)
    out = os.path.join(data, "runs", label)
    os.makedirs(out)  # refuses a label already used
    folder = os.path.join(out, "folder")
    make_folder(folder)
    hid = seed_agent(c)
    t0 = time.time()
    sid, facts = setup(c, task, folder, hid)
    t1 = time.time()
    r = c.ask(PROMPTS[task], session=sid, agent=agent, model=model)
    tid = r["turn"]
    t = c.wait_turn(tid, 1200)
    timed_out = t is None
    if timed_out:  # stop it, and say so in the result
        c.post("turn_stop", id=tid)
        t = c.wait_turn(tid, 120)
    diff, events = c.get("turn_diff", id=tid), c.get("turn_events", id=tid, session=sid)
    k = check(c, task, folder, tid, (t or {}).get("reply"), facts)
    res = dict(agent=agent, model=model, task=task, label=label, prompt=PROMPTS[task], session=sid, test_turn=tid,
               facts=facts, turn=t, timed_out=timed_out, diff=diff, events=events, check=k,
               seed_seconds=round(t1 - t0), test_seconds=round(time.time() - t1))
    if task == 4 and not k["done"]:
        try:
            c.post("turn_stop", id=facts["long_turn"])
        except RuntimeError as e:
            res["cleanup_error"] = str(e)
    if nofollow:
        # Saves the vendor's quota: stops what the test turn asked for and the
        # turns its results start in the session. Measured above, before this.
        stopped = []
        for _ in range(10):
            for t in c.all_turns():
                mine = ((t.get("origin") or {}).get("turn") == tid) or t.get("session") == sid
                if mine and t.get("state") in LIVE and t["id"] != tid:
                    try:
                        c.post("turn_stop", id=t["id"])
                        stopped.append(t["id"])
                    except RuntimeError as e:
                        stopped.append(f"{t['id']}: {e}")
            time.sleep(3)
        res["stopped_followups"] = stopped
    res["idle_after"] = c.wait_idle(sid, limit=600)
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(res, f, indent=1)
    print(label, tid, (t or {}).get("state"), "done" if k["done"] else "not done")


# ---- show, table -----------------------------------------------------------

def api_calls(res):
    """Each AgentBus API call the test turn made, with whether it failed. A
    call is an agentbus-api (or curl to the service) command; a shell line
    with several counts each."""
    outs = {e.get("tool_id"): e.get("tool_output") or "" for e in res["events"]["events"] if e["kind"] == "tool.completed"}
    calls = []
    for x in res["diff"]["tools"]["calls"]:
        o = outs.get(x["id"], "")
        for cmd in x.get("commands") or []:
            n = len(re.findall(r"agentbus-api |agentbus api |curl ", cmd))
            if not n:
                continue
            failed = bool(re.search(r"agentbus api: status [45]\d\d|requires approval|\"exit_code\":[1-9]|^Exit code [1-9]", o))
            calls.append(dict(tool=x["name"], command=cmd, n=n, failed=failed, output=o[:300]))
    return calls


def first_tool(res):
    for x in res["diff"]["tools"]["calls"]:
        cmd = " ".join(x.get("commands") or [])
        if re.search(r"agentbus-api |agentbus api |curl ", cmd):
            return "agentbus-api"
        if x["name"] in ("Agent", "Task", "ListAgents", "SendMessage", "spawn_agent", "send_input"):
            return "built-in " + x["name"]
        return x["name"] + (": " + cmd[:40] if cmd else "")
    return "none"


def summary(res):
    t, u = res.get("turn") or {}, (res.get("turn") or {}).get("usage") or {}
    calls = api_calls(res)
    return dict(label=res["label"], task=res["task"], done=res["check"]["done"], first=first_tool(res),
                api_calls=sum(x["n"] for x in calls), api_failed=sum(1 for x in calls if x["failed"]),
                tool_calls=len(res["diff"]["tools"]["calls"]), model_calls=t.get("model_calls"),
                tokens_in=(u.get("input_tokens") or 0) + (u.get("cache_read_tokens") or 0) + (u.get("cache_write_tokens") or 0),
                tokens_out=u.get("output_tokens"), cost_usd=t.get("cost_usd"), seconds=res.get("test_seconds"),
                state=t.get("state"), timed_out=res.get("timed_out"))


def show(args):
    for p in args:
        res = json.load(open(p))
        s, t = summary(res), res.get("turn") or {}
        print("=" * 90)
        print(json.dumps(s))
        print("reply:", (t.get("reply") or "")[:1200])
        outs = {e.get("tool_id"): e.get("tool_output") or "" for e in res["events"]["events"] if e["kind"] == "tool.completed"}
        for x in res["diff"]["tools"]["calls"]:
            what = " ;; ".join(x.get("commands") or []) or json.dumps(x.get("input"))[:200]
            print(f"  - {x['name']}: {what[:300]}\n      -> {outs.get(x['id'], '')[:240]!r}")
        print("check:", json.dumps(res["check"])[:600])


def table(args):
    data = os.path.abspath(flag(args, "-dir"))
    rows = []
    for label in sorted(os.listdir(os.path.join(data, "runs"))):
        p = os.path.join(data, "runs", label, "result.json")
        if os.path.exists(p):
            rows.append(summary(json.load(open(p))))
    cols = ["label", "task", "done", "first", "api_calls", "api_failed", "tool_calls", "model_calls",
            "tokens_in", "tokens_out", "cost_usd", "seconds", "state"]
    print("| " + " | ".join(cols) + " |")
    print("|" + "---|" * len(cols))
    for r in rows:
        print("| " + " | ".join(str(r[c]) for c in cols) + " |")


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    cmd, args = sys.argv[1], sys.argv[2:]
    {"up": up, "agents": agents, "run": run, "show": show, "table": table, "down": down}.get(cmd, lambda a: sys.exit(__doc__))(args)


if __name__ == "__main__":
    main()
