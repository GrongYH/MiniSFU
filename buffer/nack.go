package buffer

type nack struct {
	sn     uint32
	nacked uint8
}

type nackQueue struct {
	nacks   []nack
	counter uint8
	maxSN   uint16
	kfSN    uint32
	cycles  uint32
}
