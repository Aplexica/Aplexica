package proto

const (
	MaxJSONRPCFrameBytes       = 64 << 20
	MaxSealedEventBytes        = 4 << 20
	MaxInboundEvents           = 100
	MaxInboundBytes            = 32 << 20
	MaxPublishEvents           = 4
	MaxPublishBytes            = 32 << 20
	MaxRecipientWraps          = 256
	MaxRemoteDecompressedBytes = 64 << 20
	MaxDeliveryIDBytes         = 256
	MaxDurableCursorBytes      = 4096
	MaxMetadataBytes           = 128
	MaxPluginErrorBytes        = 512
)
