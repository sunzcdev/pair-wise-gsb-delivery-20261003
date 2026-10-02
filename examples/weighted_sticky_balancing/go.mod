module weighted_sticky_balancing

go 1.27.0

require (
	github.com/twmb/franz-go v1.21.7
	github.com/twmb/franz-go/pkg/kmsg v1.14.0
)

replace github.com/twmb/franz-go => ../../

require (
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.30 // indirect
)
