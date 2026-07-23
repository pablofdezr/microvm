module github.com/pablofdezr/microvm

go 1.26

require (
	github.com/mdlayher/vsock v1.2.1
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/sys v0.30.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.14 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.29 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.4.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.32.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.37.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.44.1 // indirect
	github.com/aws/smithy-go v1.27.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/mdlayher/socket v0.4.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/net v0.17.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
)

require (
	github.com/aws/aws-sdk-go-v2 v1.42.1
	github.com/aws/aws-sdk-go-v2/config v1.32.30
	github.com/aws/aws-sdk-go-v2/service/s3 v1.105.2
	github.com/hanwen/go-fuse/v2 v2.11.0
	github.com/pablofdezr/microvm-sdk-go v0.0.0
	github.com/redis/go-redis/v9 v9.21.0
)

// The SDK is a separate module so clients do not compile netlink and vsock.
// The CLI is the one thing in this repo that is a client, so it points at the
// local copy rather than a published version.
replace github.com/pablofdezr/microvm-sdk-go => ./sdk/go
