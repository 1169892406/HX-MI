module homex.dev/plugins/xiaomi-miot

go 1.24.0

require homex.dev/homex/sdk/go v0.0.0

replace homex.dev/homex/sdk/go => ../../../sdk/go

require qrlogin v0.0.0

require rsc.io/qr v0.2.0 // indirect

replace qrlogin => ./qrlogin
