module solastat/ext

go 1.25.4

// The app itself (solastat/internal/extapi); this module is only built as
// a dependency of solastat, the main module.
require (
	github.com/OrbitOS-org/orbit-os-sdk-go/v26 v26.1.0
	github.com/pedronsg/solastat-template v0.0.0
	solastat v0.0.0
)

replace solastat => ../

replace github.com/OrbitOS-org/orbit-os-sdk-go/v26 => ../orbit-os-sdk-go

replace github.com/pedronsg/solastat-template => ../plugins/template
