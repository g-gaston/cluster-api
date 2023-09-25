package inplaceupgrade

import "strings"

func isConnectionRefusedAPIServer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

func isConnectionResetAPIServer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection reset by peer")
}

func isPodInitializing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is waiting to start: PodInitializing")
}

func isWeirdErrorWhileEtcdRestarts(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Internal error occurred: Authorization error")
}

func isEOFFromAPI(err error) bool {
	return err != nil && strings.Contains(err.Error(), "EOF")
}
