# remote: one complete remote path, across two machines

`tools/remote` runs AgentBus's remote path end to end and times each step. Control (`agentbus
server`) and a connector run on this Mac; a runner runs in a Lima VM. A stand-in agent edits files,
so no model, key or sign-in is involved. It is not part of the release binary.

## Run it

From the framework's folder, with the VM running:

```sh
go run ./tools/remote
```

Flags:

- `-vm agentbus-dev`: the Lima VM the runner runs in.
- `-vm-dir /work/remote-runner`: the runner's scratch folder in the VM. It must not exist; it is
  made, and removed at the end.
- `-port 8797`: the server's port on 127.0.0.1. The tool stops if it is in use.
- `-runs 3`: how many times the path runs, each in a new session.
- `-sizes 300,3000,50000`: folder sizes, in files, whose snapshot transfer is measured. Empty for
  none.
- `-dir <path>`: the Mac scratch folder (default: a new temp folder), removed at the end.
- `-keep`: keep both scratch folders.
- `-hold`: after the checks, keep everything running until Ctrl-C, to look at it in a cockpit.
  The token is in `<dir>/server/token`.
- `-network`: after the checks, probe the runner's network control from a turn (below).
- `-allow github.com`: with `-network`, the one host in the server's `[network] allow`.
- `-real`: after the checks, a real model turn on a runner that holds no secret (below).
- `-connect`: with `-real`, first connect your claude subscription on the scratch server: it runs
  `agentbus connect claude` in this terminal (`claude setup-token`, then you sign in).
- `-key-env OPENROUTER_API_KEY`: with `-real`, the variable the server reads the OpenRouter key
  from, by its name. The tool checks only whether it is set; the server inherits it.
- `-model deepseek/deepseek-v4.1-flash`: with `-real`, the OpenRouter agent's model.

## A real model turn (-real)

A second runner starts in the VM, `-labels real`, with the VM's own `claude` on its `PATH`, by
`env -i` as the first: no key, no sign-in, a `HOME` of its own. It runs two turns, each asking for
one word:

1. An OpenRouter agent (`key_env` as `-key-env`). The server reads the key from its environment.
2. A claude subscription agent, with `subscriptions_on_runners = "own"` set on the scratch server.
   It uses the subscription connected there (`-connect`), or is refused when none is.

Each turn's agent gets its turn token as its credential. Its model calls go through the runner's
gateway to the server's relay (`/relay/...`), which adds the real one. The tool prints how each
job ended, the turn's reply or error, the relay's answers by status, and when the server's record
first held the turn's model call while it ran. Without the key, or without a connected
subscription, the relay answers 403 and the turn fails; that still shows the path.

Your own run, with the key exported in your shell under its usual name:

```sh
go run ./tools/remote -runs 1 -sizes "" -real -connect
```

Ctrl-C at any point stops what the tool started, on both machines.

## Network control (-network)

The server's policy gets `[network] contain = "on"` and `allow = ["<-allow>"]`, and one turn asks the stand-in for
`network probe`. From inside its turn it tries, with curl and getent:

- its runner's gateway (any HTTP answer means it got there);
- `https://<-allow>/` through the proxy: let through;
- `https://example.com/`: refused by the proxy (CONNECT 403);
- `https://1.1.1.1/` through the proxy (refused, 403) and straight (no route: no answer);
- a DNS lookup of `example.com`: fails, there is no DNS.

Each line is `<name> <http code>/<CONNECT code>`; `000` is no answer. Then the tool prints the
turn's `network.refused` rows from the server's record. On a runner that cannot close an agent off,
the turn is refused, and the tool prints the job's reason instead. With `-real`, each real turn also
lists its refusals by host.

## What it sets up

- Two builds of the current source: one for the Mac, one for the VM (`GOOS=linux`, `CGO_ENABLED=0`).
- On the Mac, in the scratch folder: a small git project, a fixture folder per size (text files of
  200 B to 16 KB, as `tools/bench` writes them), `agentbus server -listen 127.0.0.1:<port> -host
  host.lima.internal`, and `agentbus connector` owning those folders. Nothing is installed.
- In the VM: `agentbus runner -server http://host.lima.internal:<port>`, started with `env -i`, so
  it has only its own `HOME`, `PATH` and data folder: no key or sign-in of the VM's user reaches
  it. Its `PATH` holds the stand-in `claude`. It holds no secret (`-gateway server`, the default).

`-host host.lima.internal` is needed because the VM reaches the Mac's loopback by that name, and a
service on loopback refuses a `Host` it does not know.

## What it checks, each run

1. A new session on the project, isolated: the connector takes a snapshot, sends the blobs the
   server lacks, and the VM runner builds its copy from them. The stand-in appends to `a.txt` and
   adds `new.txt`.
2. A later turn in the same session: it appends again and removes `new.txt`.
3. The Mac folder is untouched.
4. What a cockpit reads from the server holds both turns: `GET /api/v0/sessions` lists the session;
   `/turns?session=`, `/turns/<id>` and `/turns/<id>/diff` show each turn finished, with its
   files.
5. Apply up to the later turn: both turns in one step. The Mac folder then has `a.txt` as the
   session left it and no `new.txt`. The same apply again writes nothing.
6. A new session; the tool edits `a.txt` on the Mac; the apply is refused with `a.txt` and why,
   and nothing is written.

Any check that fails stops the tool with what it saw.

## The numbers

Each cell is the median of the runs, with the worst in brackets, in ms.

- **total (client)**: `POST /api/v0/turns` to the job seen ended.
- **queue**: job created to taken (server clock).
- **snapshot**: the snapshot job, created to ended: the connector hashes the folder (a stat cache
  keeps unchanged files' hashes) and sends what the server lacks (server clock).
- **copy**: the runner got the snapshot to its copy ready: it fetches what it lacks and builds the
  copy (VM clock). Without a snapshot, from the attempt's start.
- **start**, **agent**, **checkpoint**: as in `tools/bench` (VM clock).
- **finish**: the VM's last event to the job ended on the server: the changeset and its files sent,
  the turn's record sent, the finish. Two clocks: corrected by the middle of the measured skew,
  which the tool prints at setup.
- **cockpit view**, **apply**: the calls' round trips on the client.

The transfer table measures a new session's first turn on each fixture twice. **cold**: no blob of
it is held by the server or the runner yet. **warm**: a second session on the same folder, so the
server and the runner hold every blob. **blob requests** counts the server's blob and pack
requests in that turn, from its request log.
