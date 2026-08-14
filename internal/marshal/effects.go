package marshal

// EffectKind names a high-level external operation at the point where it can
// still be denied before Git or network work begins.
type EffectKind string

const (
	EffectSyncBase    EffectKind = "sync-base"
	EffectLand        EffectKind = "land"
	EffectPublish     EffectKind = "publish"
	EffectDeleteBranch EffectKind = "delete-branch"
)

// Effect identifies an externally observable merge-train operation.
type Effect struct {
	Kind   EffectKind
	Target string
}

// EffectSink admits operations before execution and observes their completion.
// A nil sink preserves the production merge-train behavior.
type EffectSink interface {
	Admit(Effect) error
	Complete(Effect, error)
}
