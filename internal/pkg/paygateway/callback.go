package paygateway

// MaxCallbackBodyBytes is shared by payment and refund callback handlers. The
// bounded 1.25 MiB limit leaves room for APIv3 notification envelopes while
// preventing unbounded request-body allocation.
const MaxCallbackBodyBytes int64 = 1280 * 1024
