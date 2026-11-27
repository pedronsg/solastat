module solastat

go 1.25.4

require (
	github.com/OrbitOS-org/orbit-os-sdk-go/v26 v26.1.0
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/pedronsg/solastat-template v0.0.0
	modernc.org/sqlite v1.50.0
	solastat/ext v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251022142026-3a174f9686a8 // indirect
	google.golang.org/grpc v1.77.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	modernc.org/libc v1.72.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/OrbitOS-org/orbit-os-sdk-go/v26 => ./orbit-os-sdk-go

replace github.com/pedronsg/solastat-template => ./plugins/template

// The app's extension: the default one, ext/, adds nothing; a build can
// replace it in a go.work with its own.
replace solastat/ext => ./ext
