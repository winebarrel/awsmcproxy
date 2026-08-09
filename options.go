package awsmcproxy

// Options holds the command-line options.
type Options struct {
	Config string `kong:"required,short='c',env='AWSMCPROXY_CONFIG',help='Config file path.'"`
}
