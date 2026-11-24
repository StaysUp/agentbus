# Changelog

All notable changes to AgentBus. Full notes for each release are on the
[releases page](https://github.com/staysup/agentbus/releases).

## Unreleased

### Changed
- **Rules allow everything by default except subagents.** The default Allow list is now `*` (every
  command) and `tool:*` (every tool), Ask is empty, and Forbid holds `tool:subagent`. `*` matches
  every command and `tool:*` allows every tool; Forbid still wins over Ask, Ask over Allow. A command
  or tool on none of the lists is held for a person. Narrow the lists to hold or refuse what you
  choose. The redundant `tool:edit` default is gone (editing runs under `tool:*`).
- **The `unlisted` setting is retired.** A command or tool on none of the lists is always held for a
  person now. An older `permissions.toml` or stored policy that still sets `unlisted` keeps working,
  with the key left out.
- The folder tools `read_file`, `list_folder` and `write_file` are hidden: they are no longer offered
  to agents or listed in Settings or the turn view. The HTTP routes stay for a later release.
- When a runner stops answering mid-turn, the turn ends with a plain message: "The runner stopped
  answering; send the turn again."
- **The runners API no longer reports `sandbox`.** It said whether a machine could build the
  whole-agent wall retired in 0.6.4, and decided nothing. A Copy session walls each command through
  the vendor's own sandbox. A runner of an older build may still send the field; control ignores it.

### Fixed
- `tools/bench` applies through `POST /api/v0/workspaces/{id}/apply`, the route that replaced the
  removed per-turn apply.

## 0.6.4 (2026-11-23)

### Added
- **Use any model.** Claude Code can run on any model OpenRouter offers; the agent stays Claude
  Code, the model behind it changes.
- **OpenCode** joins as a harness, on an OpenRouter key.
- AgentBus records OpenRouter's Chat Completions format, with its cost.
- **Direct, Copy or Locked, per session.** Copy (the default) works in its own copy of your folder:
  commands write only inside the copy, reads stay open, the network and local services work as
  normal. Direct works in your folder itself. Locked comes later.
- **Workspaces, Review, Discard.** A Copy session's workspace holds its copy; read the changes, ask
  another model to check them with Review, then Apply or Discard.
- **One sign-in path (Connect) in every mode.** On `agentbus up` a turn runs in your real home, so
  your setup (`CLAUDE.md`, hooks, plugins, skills, `gh`, `aws`, `ssh`, `gitconfig`) comes along.
- A turn's record comes back in one read.

### Changed (breaking)
- Apply and preview act on a workspace: `POST /api/v0/workspaces/{id}/apply` replaces
  `POST /api/v0/turns/{id}/apply`, and `GET /api/v0/workspaces/{id}/changes` replaces
  `GET /api/v0/sessions/{id}/changes`.
- New routes: `POST /api/v0/sessions`, `POST /api/v0/workspaces`, `GET /api/v0/workspaces`,
  `GET /api/v0/workspaces/{id}`, `POST /api/v0/workspaces/{id}/discard`.
- The `[sandbox]` mode and `[network]` contain/allow settings are retired with the whole-process
  wall. An older `permissions.toml` or stored policy keeps working with those keys left out. The
  `isolation` and `home_files` settings stay.
- At startup, sessions are migrated to workspaces, idempotently and without deleting or moving
  anything on disk. Back up your data folder first.

### Fixed
- The file tools (`read_file`, `list_folder`, `write_file`) for a session whose folder is on another
  machine no longer act on control's own disk; they are refused with a 409. Thanks to Ahmed
  (`ahmed-elnashar`) for the fix (#56).
- `agentbus up` says which data folder it uses, and whether it is new.
- The install script prints the exact PATH line for your shell, with a literal `~`.
- Plainer words for two known failures (an agy sign-in that needs a retry, a sandbox the machine
  cannot build).
- Ask-flow cleanups.

### Cockpit
- A Direct / Copy / Locked selector in New Session (Locked disabled, "coming later").
- Review beside Apply in the changes panel; workspace status; Discard on a Copy session.
- Settings drops the Isolation group; it is now Subscriptions, Commands, Limits and Config.

## 0.6.3 (2026-11-20)

### Added
- **Archive replaces close.** Archiving hides a session from the list without deleting anything; it
  can be unarchived, and works even while a turn runs. Closing no longer deletes a session's copy.
- The record works as an event log that readers follow from where they left off; an event a reader
  keeps failing on is set aside rather than blocking the rest.

### Cockpit
- Archive and Unarchive replace Close; the list hides archived sessions, and Show archived brings
  them back.
</content>
