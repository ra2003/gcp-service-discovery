package targetsource

// TargetSource defines the interface for collecting targets from various
// services. New services should implement this interface.
type TargetSource interface {
	// Collect retrieves all targets from a source.
	Collect() error

	// Save writes the targets to the named file.
	Save() error
}

// Factory defines the interface for creating new TargetSource instances.
type Factory interface {
	// Client creates a new TargetSource ready for collection.
	Client() (TargetSource, error)
}
