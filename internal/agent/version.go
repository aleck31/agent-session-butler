package agent

// Version identifies this build to agents we talk to (Codex's app-server asks for
// a clientInfo). Set once at startup by main so the constant lives in one place.
var Version = "dev"
