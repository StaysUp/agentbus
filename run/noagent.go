package run

// NoAgent is why a turn that names no agent is refused: every turn names
// the saved agent it runs on, a session's next turn too (his rule,
// 2026-11-12).
const NoAgent = "agent is required: name the saved agent this turn runs on (GET /api/v0/agents lists them)"
