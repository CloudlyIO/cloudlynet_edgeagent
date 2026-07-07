package cwmp

import (
	"strings"
	"testing"
)

// Fixtures are built from the TR-069 spec + the NanoLink manifest/observed
// values. No real pcap exists in the nybsys materials (searched — see
// acs_gap/notes.md); real-bytes round-trip proof is deferred to on-device
// validation. Decode is Strict=false so the device's cwmp:/soapenv: prefixes
// match the un-prefixed struct tags by local name.

const informXML = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">100</cwmp:ID></soapenv:Header>
<soapenv:Body>
<cwmp:Inform>
<DeviceID>
<Manufacturer>NybSys</Manufacturer>
<OUI>8C1F64</OUI>
<ProductClass>ENB-N03002-B3</ProductClass>
<SerialNumber>2205600282</SerialNumber>
</DeviceID>
<Event soap:arrayType="cwmp:EventStruct[1]">
<EventStruct><EventCode>2 PERIODIC</EventCode><CommandKey></CommandKey></EventStruct>
</Event>
<MaxEnvelopes>1</MaxEnvelopes>
<CurrentTime>2026-07-07T00:00:00Z</CurrentTime>
<RetryCount>0</RetryCount>
<ParameterList soap:arrayType="cwmp:ParameterValueStruct[2]">
<ParameterValueStruct><Name>Device.DeviceInfo.SoftwareVersion</Name><Value xsi:type="xsd:string">V1.7.1</Value></ParameterValueStruct>
<ParameterValueStruct><Name>Device.ManagementServer.ConnectionRequestURL</Name><Value xsi:type="xsd:string">http://192.168.10.2:30005/</Value></ParameterValueStruct>
</ParameterList>
</cwmp:Inform>
</soapenv:Body>
</soapenv:Envelope>`

const atcXML = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">101</cwmp:ID></soapenv:Header>
<soapenv:Body>
<cwmp:AutonomousTransferComplete>
<AnnounceURL></AnnounceURL>
<TransferURL>ftp://192.168.8.100/</TransferURL>
<IsDownload>0</IsDownload>
<FileType>4 Vendor Log File</FileType>
<FileSize>2048</FileSize>
<TargetFileName>8C1F64_DeviceLog.tgz</TargetFileName>
<FaultStruct><FaultCode>0</FaultCode><FaultString></FaultString></FaultStruct>
<StartTime>2026-07-07T00:00:00Z</StartTime>
<CompleteTime>2026-07-07T00:00:01Z</CompleteTime>
</cwmp:AutonomousTransferComplete>
</soapenv:Body>
</soapenv:Envelope>`

const gpvRespXML = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">102</cwmp:ID></soapenv:Header>
<soapenv:Body>
<cwmp:GetParameterValuesResponse>
<ParameterList soap:arrayType="cwmp:ParameterValueStruct[1]">
<ParameterValueStruct><Name>Device.Services.FAPService.1.CellConfig.LTE.RAN.RF.ReferenceSignalPower</Name><Value xsi:type="xsd:string">-8</Value></ParameterValueStruct>
</ParameterList>
</cwmp:GetParameterValuesResponse>
</soapenv:Body>
</soapenv:Envelope>`

const gpnRespXML = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Body>
<cwmp:GetParameterNamesResponse>
<ParameterList>
<ParameterInfoStruct><Name>Device.ManagementServer.PeriodicInformInterval</Name><Writable>1</Writable></ParameterInfoStruct>
<ParameterInfoStruct><Name>Device.DeviceInfo.SerialNumber</Name><Writable>0</Writable></ParameterInfoStruct>
</ParameterList>
</cwmp:GetParameterNamesResponse>
</soapenv:Body>
</soapenv:Envelope>`

const faultXML = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Body>
<soapenv:Fault>
<faultcode>Client</faultcode>
<faultstring>CWMP fault</faultstring>
<detail><cwmp:Fault><FaultCode>9000</FaultCode><FaultString>Method not supported</FaultString></cwmp:Fault></detail>
</soapenv:Fault>
</soapenv:Body>
</soapenv:Envelope>`

func TestDecodeInform(t *testing.T) {
	env, err := DecodeEnvelope([]byte(informXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Header.ID.Value != "100" {
		t.Errorf("msgID = %q, want 100", env.Header.ID.Value)
	}
	inf := env.Body.Inform
	if inf == nil {
		t.Fatal("Inform not decoded")
	}
	if inf.DeviceID.OUI != "8C1F64" || inf.DeviceID.ProductClass != "ENB-N03002-B3" || inf.DeviceID.SerialNumber != "2205600282" {
		t.Errorf("DeviceID = %+v", inf.DeviceID)
	}
	if len(inf.Event) != 1 || inf.Event[0].EventCode != "2 PERIODIC" {
		t.Errorf("Event = %+v", inf.Event)
	}
	if len(inf.ParameterList) != 2 {
		t.Fatalf("ParameterList len = %d, want 2", len(inf.ParameterList))
	}
	if inf.ParameterList[0].Name != "Device.DeviceInfo.SoftwareVersion" || inf.ParameterList[0].Value.Text != "V1.7.1" {
		t.Errorf("param[0] = %+v", inf.ParameterList[0])
	}
}

func TestDecodeATC(t *testing.T) {
	env, err := DecodeEnvelope([]byte(atcXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	atc := env.Body.AutonomousTransferComplete
	if atc == nil {
		t.Fatal("ATC not decoded")
	}
	if atc.TargetFileName != "8C1F64_DeviceLog.tgz" || atc.FileSize != 2048 {
		t.Errorf("ATC = %+v", atc)
	}
	if atc.FaultStruct.FaultCode != 0 {
		t.Errorf("ATC FaultCode = %d, want 0", atc.FaultStruct.FaultCode)
	}
}

func TestDecodeGPVResponse(t *testing.T) {
	env, err := DecodeEnvelope([]byte(gpvRespXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := env.Body.GetParameterValuesResponse
	if r == nil || len(r.ParameterList) != 1 {
		t.Fatalf("GPV resp = %+v", r)
	}
	if r.ParameterList[0].Value.Text != "-8" {
		t.Errorf("value = %q, want -8", r.ParameterList[0].Value.Text)
	}
}

func TestDecodeGPNResponse(t *testing.T) {
	env, err := DecodeEnvelope([]byte(gpnRespXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := env.Body.GetParameterNamesResponse
	if r == nil || len(r.ParameterList) != 2 {
		t.Fatalf("GPN resp = %+v", r)
	}
	if !r.ParameterList[0].Writable {
		t.Error("PeriodicInformInterval should be writable")
	}
	if r.ParameterList[1].Writable {
		t.Error("SerialNumber should not be writable")
	}
}

func TestDecodeFault(t *testing.T) {
	env, err := DecodeEnvelope([]byte(faultXML))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Body.Fault == nil {
		t.Fatal("Fault not decoded")
	}
	if env.Body.Fault.Detail.CWMPFault.FaultCode != "9000" {
		t.Errorf("cwmp FaultCode = %q, want 9000", env.Body.Fault.Detail.CWMPFault.FaultCode)
	}
}

// TestEncodeATCResponseIsEmptyNeverFault is the crux assertion: the ATC reply
// must be the empty AutonomousTransferCompleteResponse, not a Fault.
func TestEncodeATCResponseIsEmptyNeverFault(t *testing.T) {
	out, err := EncodeResponse("101", &AutonomousTransferCompleteResponse{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "<cwmp:AutonomousTransferCompleteResponse>") {
		t.Errorf("missing cwmp:AutonomousTransferCompleteResponse open tag:\n%s", s)
	}
	if !strings.Contains(s, "</cwmp:AutonomousTransferCompleteResponse>") {
		t.Errorf("missing cwmp:AutonomousTransferCompleteResponse close tag:\n%s", s)
	}
	if strings.Contains(strings.ToLower(s), "fault") {
		t.Errorf("ATC response must never contain a Fault:\n%s", s)
	}
	if !strings.Contains(s, "<cwmp:ID soapenv:mustUnderstand=\"1\">101</cwmp:ID>") {
		t.Errorf("msgID not echoed:\n%s", s)
	}
}

func TestEncodeInformResponse(t *testing.T) {
	out, err := EncodeResponse("100", &InformResponse{MaxEnvelopes: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "<cwmp:InformResponse>") || !strings.Contains(s, "</cwmp:InformResponse>") {
		t.Errorf("InformResponse root not rewritten:\n%s", s)
	}
	if !strings.Contains(s, "<MaxEnvelopes>1</MaxEnvelopes>") {
		t.Errorf("MaxEnvelopes missing:\n%s", s)
	}
}

// TestEncodeGPVRoundTrip encodes a GetParameterValues request and decodes it
// back — proving both directions agree on element names and payload.
func TestEncodeGPVRoundTrip(t *testing.T) {
	paths := []string{"Device.A", "Device.B"}
	out, err := EncodeResponse("103", &GetParameterValues{ParameterNames: paths})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), "<cwmp:GetParameterValues>") {
		t.Errorf("GPV root not rewritten:\n%s", out)
	}
	env, err := DecodeEnvelope(out)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if env.Body.GetParameterValues == nil {
		t.Fatal("GPV did not round-trip")
	}
	got := env.Body.GetParameterValues.ParameterNames
	if len(got) != 2 || got[0] != "Device.A" || got[1] != "Device.B" {
		t.Errorf("ParameterNames = %v", got)
	}
}

func TestEncodeSPVCarriesNameAndValue(t *testing.T) {
	spv := &SetParameterValues{
		ParameterList: []ParameterValueStruct{
			{Name: "Device.X.RSP", Value: ValueNode{Type: "xsd:string", Text: "-8"}},
		},
		ParameterKey: "cmd-1",
	}
	out, err := EncodeResponse("104", spv)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(out)
	for _, want := range []string{"<cwmp:SetParameterValues>", "<Name>Device.X.RSP</Name>", ">-8<", "<ParameterKey>cmd-1</ParameterKey>"} {
		if !strings.Contains(s, want) {
			t.Errorf("SPV missing %q:\n%s", want, s)
		}
	}
}
