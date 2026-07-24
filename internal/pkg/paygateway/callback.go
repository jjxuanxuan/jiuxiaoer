package paygateway

// MaxCallbackBodyBytes 由支付和退款回调处理器共用。1.25 MiB 的有界限制
// 为 APIv3 通知信封留出空间，同时防止请求体无限分配内存。
const MaxCallbackBodyBytes int64 = 1280 * 1024
