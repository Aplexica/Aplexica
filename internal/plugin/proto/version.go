package proto

// ABIVersion is the single integer (as a string) that identifies a
// breaking-change version of the plugin protocol. Daemon refuses any
// plugin whose abi_version does not exactly match. Adding optional
// fields or new methods is NOT a breaking change and does not bump.
const ABIVersion = "1"
