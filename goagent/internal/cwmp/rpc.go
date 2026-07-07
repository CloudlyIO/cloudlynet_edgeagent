package cwmp

import "encoding/xml"

// Envelope is a decoded SOAP envelope. Only the fields the NanoLink actually
// exchanges are modelled; decode is tolerant (see soap.go) so unknown members
// are ignored rather than faulting the session.
type Envelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Header  Header   `xml:"Header"`
	Body    Body     `xml:"Body"`
}

type Header struct {
	ID ID `xml:"ID"`
}

type ID struct {
	MustUnderstand string `xml:"mustUnderstand,attr"`
	Value          string `xml:",chardata"`
}

// Body carries every CWMP message we decode (CPE→ACS) or emit (ACS→CPE).
// Pointer fields let a single decode distinguish which message arrived.
type Body struct {
	// CPE → ACS (device-initiated)
	Inform                     *Inform                     `xml:"Inform"`
	GetRPCMethods              *GetRPCMethods              `xml:"GetRPCMethods"`
	TransferComplete           *TransferComplete           `xml:"TransferComplete"`
	AutonomousTransferComplete *AutonomousTransferComplete `xml:"AutonomousTransferComplete"`
	Fault                      *Fault                      `xml:"Fault"`
	// CPE → ACS (responses to our calls)
	GetParameterValuesResponse *GetParameterValuesResponse `xml:"GetParameterValuesResponse"`
	SetParameterValuesResponse *SetParameterValuesResponse `xml:"SetParameterValuesResponse"`
	GetParameterNamesResponse  *GetParameterNamesResponse  `xml:"GetParameterNamesResponse"`
	RebootResponse             *RebootResponse             `xml:"RebootResponse"`
	// ACS → CPE (we emit these)
	InformResponse                     *InformResponse                     `xml:"InformResponse,omitempty"`
	GetRPCMethodsResponse              *GetRPCMethodsResponse              `xml:"GetRPCMethodsResponse,omitempty"`
	TransferCompleteResponse           *TransferCompleteResponse           `xml:"TransferCompleteResponse,omitempty"`
	AutonomousTransferCompleteResponse *AutonomousTransferCompleteResponse `xml:"AutonomousTransferCompleteResponse,omitempty"`
	GetParameterValues                 *GetParameterValues                 `xml:"GetParameterValues,omitempty"`
	SetParameterValues                 *SetParameterValues                 `xml:"SetParameterValues,omitempty"`
	GetParameterNames                  *GetParameterNames                  `xml:"GetParameterNames,omitempty"`
	Reboot                             *Reboot                             `xml:"Reboot,omitempty"`
}

// ── CPE → ACS ───────────────────────────────────────────────────────────────

type Inform struct {
	DeviceID      DeviceID               `xml:"DeviceID"`
	Event         []EventStruct          `xml:"Event>EventStruct"`
	MaxEnvelopes  int                    `xml:"MaxEnvelopes"`
	CurrentTime   string                 `xml:"CurrentTime"`
	RetryCount    int                    `xml:"RetryCount"`
	ParameterList []ParameterValueStruct `xml:"ParameterList>ParameterValueStruct"`
}

type DeviceID struct {
	Manufacturer string `xml:"Manufacturer"`
	OUI          string `xml:"OUI"`
	ProductClass string `xml:"ProductClass"`
	SerialNumber string `xml:"SerialNumber"`
}

type EventStruct struct {
	EventCode  string `xml:"EventCode"`
	CommandKey string `xml:"CommandKey"`
}

type ParameterValueStruct struct {
	Name  string    `xml:"Name"`
	Value ValueNode `xml:"Value"`
}

// ValueNode carries the CWMP Value element's text and its xsi:type. The tag
// emits a literal `xsi:type="..."` on writes (the SOAP envelope declares the
// xsi prefix), which the TR-069 CPE ecosystem expects — Go's namespace-URL attr
// form would instead emit a generated prefix that a strict CPE may reject. On
// decode only Text is consumed, so the inbound xsi:type prefix is irrelevant.
type ValueNode struct {
	Type string `xml:"xsi:type,attr,omitempty"`
	Text string `xml:",chardata"`
}

type GetRPCMethods struct{}

type TransferComplete struct {
	CommandKey   string      `xml:"CommandKey"`
	FaultStruct  FaultStruct `xml:"FaultStruct"`
	StartTime    string      `xml:"StartTime"`
	CompleteTime string      `xml:"CompleteTime"`
}

// AutonomousTransferComplete is the RPC the former ACS could not handle — the device
// emits it after every ~60s FTP log upload. See onATC in session.go.
type AutonomousTransferComplete struct {
	AnnounceURL    string      `xml:"AnnounceURL"`
	TransferURL    string      `xml:"TransferURL"`
	IsDownload     bool        `xml:"IsDownload"`
	FileType       string      `xml:"FileType"`
	FileSize       int         `xml:"FileSize"`
	TargetFileName string      `xml:"TargetFileName"`
	FaultStruct    FaultStruct `xml:"FaultStruct"`
	StartTime      string      `xml:"StartTime"`
	CompleteTime   string      `xml:"CompleteTime"`
}

type FaultStruct struct {
	FaultCode   int    `xml:"FaultCode"`
	FaultString string `xml:"FaultString"`
}

// Fault is a CPE-originated SOAP fault.
type Fault struct {
	FaultCode   string `xml:"faultcode"`
	FaultString string `xml:"faultstring"`
	Detail      struct {
		CWMPFault CWMPFault `xml:"Fault"`
	} `xml:"detail"`
}

type CWMPFault struct {
	FaultCode   string `xml:"FaultCode"`
	FaultString string `xml:"FaultString"`
}

type GetParameterValuesResponse struct {
	ParameterList []ParameterValueStruct `xml:"ParameterList>ParameterValueStruct"`
}

type SetParameterValuesResponse struct {
	Status int `xml:"Status"`
}

type GetParameterNamesResponse struct {
	ParameterList []ParameterInfoStruct `xml:"ParameterList>ParameterInfoStruct"`
}

type ParameterInfoStruct struct {
	Name     string `xml:"Name"`
	Writable bool   `xml:"Writable"`
}

type RebootResponse struct{}

// ── ACS → CPE ───────────────────────────────────────────────────────────────

type InformResponse struct {
	MaxEnvelopes int `xml:"MaxEnvelopes"`
}

type GetRPCMethodsResponse struct {
	MethodList []string `xml:"MethodList>string"`
}

type TransferCompleteResponse struct{}

// AutonomousTransferCompleteResponse is THE FIX: an empty body that MUST be
// returned (never a Fault) so the CWMP session survives the CPE's ATC RPC and
// proceeds to the ACS's read/write turn.
type AutonomousTransferCompleteResponse struct{}

type GetParameterValues struct {
	ParameterNames []string `xml:"ParameterNames>string"`
}

type SetParameterValues struct {
	ParameterList []ParameterValueStruct `xml:"ParameterList>ParameterValueStruct"`
	ParameterKey  string                 `xml:"ParameterKey"`
}

type GetParameterNames struct {
	ParameterPath string `xml:"ParameterPath"`
	NextLevel     bool   `xml:"NextLevel"`
}

type Reboot struct {
	CommandKey string `xml:"CommandKey"`
}
