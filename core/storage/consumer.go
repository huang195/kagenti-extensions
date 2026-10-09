package storage

// StoreConsumer is implemented by plugins that keep state which must outlive the
// process. plugins.BuildWithDeps injects the process Store before Configure runs,
// as it injects spiffe.ProviderConsumer's and pricing.ResolverConsumer's.
//
// A plugin must tolerate never being called: a process with no store to offer — any
// that is not a local install — passes nothing, and the plugin then keeps its state
// in memory as before.
type StoreConsumer interface {
	SetStore(Store)
}
