# bench: AgentBus's own overhead on one machine

`tools/bench` measures what AgentBus adds around an agent when one person runs it on one machine
(`agentbus up`). It is not part of the release binary.

It starts its own `agentbus up` on a free port with a scratch data folder, and a stand-in agent in
place of `claude`. The stand-in appends a line to three files, adds or removes one file, and exits
at once. So no number here holds model time. At the end the bench stops the service and removes the
scratch folder.

## Run it

```sh
go build -o /tmp/agentbus ./cmd/agentbus
go run ./tools/bench -agentbus /tmp/agentbus
```

Flags:

- `-sizes 300,3000,50000`: folder sizes, in files.
- `-runs 5`: sessions per size. Each session is two turns.
- `-warmup 1`: sessions run first on each size and left out of the numbers. The first session
  on a fixture just written was much slower in a trial (1.7 s more at 3,000 files, on APFS), and a
  person's folder is not one just written.
- `-port 8790`: the scratch service's port on 127.0.0.1. The bench stops if it is in use.
- `-dir <path>`: the scratch folder, new or empty: the bench removes it at the end. Default: a new
  temp folder. Put it on the disk you want to measure: the session copies are clones only on the
  disk that holds the folder.
- `-keep`: keep the scratch folder.
- `-json <file>`: also write every run's numbers.

To compare two builds, run the bench with each binary on the same machine, one after the other,
and compare the medians. A busy machine moves the worst column most.

## What it does

For each size it writes one fixture of text files (200 B to 16 KB, the same on every machine,
as `gNN/dN/fNNN.txt`). Each run clones the fixture into a new folder (`cp -c` on macOS,
`cp --reflink=auto` on Linux) and runs one session there:

1. `POST /api/v0/turns` with a new session. The bench polls `GET /api/v0/jobs/{id}` every 2 ms
   until the job has ended.
2. `POST /api/v0/turns` with that session: the later turn.
3. `GET /api/v0/turns/{id}/diff` for each turn.
4. `POST /api/v0/turns/{id}/apply` for the first turn, then the later one. The bench checks the
   folder ends without the file the first turn added and the later one removed.

The service runs with `-replay=false`. Own replay continues a session from its stored model calls,
and the stand-in makes none. So the later turn resumes by the result line the stand-in prints. The
file work of a turn is the same either way.

## The numbers

Each cell is the median of the runs, with the worst in brackets.

- **total**: from sending `POST /turns` to seeing the job ended, on the client's clock.
- **POST reply**: until the `POST /turns` reply, which comes once the runner has started the turn.
- The rest come from the job's own times and its attempt's events
  (`GET /api/v0/attempts/{id}/events`), on the service's clock:
  - **queue**: job created to taken by the runner.
  - **copy** (first turn only): attempt started to `workspace.ready`. The session's copy of the
    folder and its first checkpoint.
  - **start**: to `turn.created`. The turn's start checkpoint and the agent's process started.
  - **agent**: `turn.created` to `turn.ended`. The stand-in itself takes a few ms.
  - **checkpoint**: `turn.ended` to `workspace.changes`. The end checkpoint and the diff.
  - **finish**: `workspace.changes` to the job ended.
- **diff** and **apply**: the call's round trip on the client.
- **RSS idle**: the service's resident memory before each session; **RSS peak in turns**: the
  highest of samples every 50 ms during the two turns. Both sum every process of the service (on
  Linux `up` runs in a user namespace, as a second process), without the stand-in's.
- **disk**: blocks allocated under the data folder, without the session copies themselves (a clone
  or an overlay shares the folder's blocks). Blocks shared by reflinks inside the data folder, such
  as an overlay's lower layer, are counted in full. **free space lost** is the disk's own free
  space before and after the session. Other programs on the same disk move it.
