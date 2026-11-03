package wire

// User-facing texts more than one package prints (spec 9 owns the wording).
const (
	TextListenerDown  = "kwak listener is not running. Start it with: kwak install (or systemctl --user start kwak). Details: kwak doctor"
	TextListenerOlder = "the kwak listener is older than kwak; run kwak install to restart it"
	TextUnpaired      = "this machine's kwak pairing was revoked or expired; run kwak pair"
	TextNotPaired     = "this machine is not paired; run kwak pair"
	TextKeyMissing    = "the kwak key is missing from the keyring; run kwak pair"
)
