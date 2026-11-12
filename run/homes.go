package run

// NoHomes is why a runner cannot sandbox a session when it gives sessions
// no home of their own (Config.Homes is empty): the wall needs one. The
// runner's sandbox report says it, and so does a turn's refusal.
const NoHomes = "this runner gives sessions no home of their own (a runner with -gateway local runs them in this user's home)"

// NoAgent is why a turn that names no agent is refused: every turn names
// the saved agent it runs on, a session's next turn too (his rule,
// 2026-11-12).
const NoAgent = "agent is required: name the saved agent this turn runs on (GET /api/v0/agents lists them)"
