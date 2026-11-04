package usbmux

// DeviceProperties mirrors the Properties dictionary usbmuxd attaches to each
// device. Only the fields Crossmap needs to route or re-advertise are named;
// the rest ride along in Extra so a proxied Attached event stays faithful to
// what the origin host reported.
type DeviceProperties struct {
	ConnectionType  string `plist:"ConnectionType"`
	ConnectionSpeed int    `plist:"ConnectionSpeed,omitempty"`
	DeviceID        int    `plist:"DeviceID"`
	LocationID      int    `plist:"LocationID,omitempty"`
	ProductID       int    `plist:"ProductID,omitempty"`
	SerialNumber    string `plist:"SerialNumber"`
	UDID            string `plist:"UDID,omitempty"`
	EscrowBag       []byte `plist:"EscrowBag,omitempty"`
	NetworkAddress  []byte `plist:"NetworkAddress,omitempty"`
	InterfaceIndex  int    `plist:"InterfaceIndex,omitempty"`
}

// Udid returns the device's unique identifier, tolerating hosts that populate
// only one of the two keys usbmuxd has used over the years.
func (p DeviceProperties) Udid() string {
	if p.SerialNumber != "" {
		return p.SerialNumber
	}
	return p.UDID
}

// Event is an Attached or Detached notification from a Listen stream.
type Event struct {
	MessageType string           `plist:"MessageType"`
	DeviceID    int              `plist:"DeviceID"`
	Properties  DeviceProperties `plist:"Properties,omitempty"`
}

// Request is the common shape of every client-to-usbmuxd message. Fields are
// omitted when empty so a single struct can serve every request type.
type Request struct {
	MessageType         string `plist:"MessageType"`
	ClientVersionString string `plist:"ClientVersionString,omitempty"`
	ProgName            string `plist:"ProgName,omitempty"`
	LibUSBMuxVersion    int    `plist:"kLibUSBMuxVersion,omitempty"`
	DeviceID            int    `plist:"DeviceID,omitempty"`
	PortNumber          int    `plist:"PortNumber,omitempty"`
	PairRecordID        string `plist:"PairRecordID,omitempty"`
	PairRecordData      []byte `plist:"PairRecordData,omitempty"`
}

// Result is usbmuxd's acknowledgement of a request.
type Result struct {
	MessageType string `plist:"MessageType"`
	Number      int    `plist:"Number"`
}

// DeviceList is the reply to ListDevices.
type DeviceList struct {
	DeviceList []Event `plist:"DeviceList"`
}

// PairRecord is the reply to ReadPairRecord.
type PairRecord struct {
	PairRecordData []byte `plist:"PairRecordData"`
}

// BUID is the reply to ReadBUID.
type BUID struct {
	BUID string `plist:"BUID"`
}

// swap16 converts a port between host and network byte order. usbmuxd's
// PortNumber field carries a big-endian port inside a little-endian integer,
// so callers must pre-swap.
func swap16(p int) int {
	return ((p << 8) & 0xff00) | ((p >> 8) & 0xff)
}
