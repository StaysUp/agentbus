# Security

Thank you for helping keep AgentBus safe.

## Report a vulnerability privately

Email **security@staysup.io**. Do not open a public issue for a security problem.

Please include:

- What the problem is, and what an attacker could do with it.
- The steps to reproduce it, with the smallest example you can.
- The version (`agentbus version`) and the operating system.
- Any logs or output that show it, with secrets removed.

We reply in writing. We will confirm we received your report, tell you what we find, and let you
know when a fix is out.

## In scope

- **The gateway.** It sits between the agents and the model vendors and carries the user's sign-in.
- **Sign-ins and keys.** How AgentBus stores subscription sign-ins and API keys, and keeps them
  out of events, logs and files.
- **The Copy wall.** A Copy session's commands write only inside its copy, plus temporary files and
  the session's own cache folder, which Close removes. They never write the person's own cache
  folders. Its network is held to an allow-list for the sandbox-engine CLIs. What keeps those
  boundaries, and any way around them.
- **The API token.** The token that guards the API, and anything that would let a caller skip it.
