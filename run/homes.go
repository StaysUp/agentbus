package run

// NoHomes is why a runner cannot sandbox a session when it gives sessions
// no home of their own (Config.Homes is empty): the wall needs one. The
// runner's sandbox report says it, and so does a turn's refusal.
const NoHomes = "this runner gives sessions no home of their own (agentbus up, and a runner with -gateway local, run them in this user's home)"
