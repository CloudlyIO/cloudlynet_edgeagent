// Device -> agent SOAP builders: the CWMP envelopes the mock device sends
// (Inform, GetRPCMethods, TransferComplete, AutonomousTransferComplete) and the
// responses it returns to the ACS's queued tasks (GPV/SPV/GPN/Reboot).
package main

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

const soapOpen = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">` + "%s" + `</cwmp:ID></soapenv:Header>
<soapenv:Body>`

const soapClose = `</soapenv:Body></soapenv:Envelope>`

func wrap(msgID, inner string) string {
	return fmt.Sprintf(soapOpen, msgID) + inner + soapClose
}

func informEnvelope(msgID string, dev *device) string {
	informParams := []string{
		"Device.DeviceInfo.SoftwareVersion",
		"Device.LAN.IPAddress",
		"Device.WAN.IPAddress",
		"Device.Services.FAPService.1.FAPControl.LTE.RFTxStatus",
		"Device.Services.FAPService.1.FAPControl.LTE.OpState",
	}
	// Fleet mode: each device advertises its own connection-request URL
	// (30005+index) in the Inform, like a real CPE. Single-device mode keeps
	// the historical parameter list byte-for-byte (crURL is empty).
	if dev.crURL != "" {
		informParams = append(informParams, connectionRequestURLPath)
	}
	pairs := dev.get(informParams)
	var pl strings.Builder
	for _, pv := range pairs {
		fmt.Fprintf(&pl, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="xsd:string">%s</Value></ParameterValueStruct>`, pv[0], pv[1])
	}
	inner := fmt.Sprintf(`<cwmp:Inform>`+
		`<DeviceID><Manufacturer>NybSys</Manufacturer><OUI>%s</OUI><ProductClass>%s</ProductClass><SerialNumber>%s</SerialNumber></DeviceID>`+
		`<Event soap:arrayType="cwmp:EventStruct[1]"><EventStruct><EventCode>2 PERIODIC</EventCode><CommandKey></CommandKey></EventStruct></Event>`+
		`<MaxEnvelopes>1</MaxEnvelopes><CurrentTime>%s</CurrentTime><RetryCount>0</RetryCount>`+
		`<ParameterList soap:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList>`+
		`</cwmp:Inform>`,
		dev.oui, dev.productClass, dev.serial, time.Now().UTC().Format(time.RFC3339), len(pairs), pl.String())
	return wrap(msgID, inner)
}

// getRPCMethodsEnvelope is a device-initiated notification (CPE -> ACS); the
// agent's onGetRPCMethods (session.go) answers with its supported method list.
func getRPCMethodsEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:GetRPCMethods></cwmp:GetRPCMethods>`)
}

// transferCompleteEnvelope is the CPE's notification that an ACS-commanded
// transfer finished; the agent's onTransferComplete (session.go) answers with
// an empty TransferCompleteResponse. Distinct from AutonomousTransferComplete
// (atcEnvelope), which is for transfers the device initiated on its own.
func transferCompleteEnvelope(msgID string) string {
	now := time.Now().UTC().Format(time.RFC3339)
	inner := `<cwmp:TransferComplete>` +
		`<CommandKey></CommandKey>` +
		`<FaultStruct><FaultCode>0</FaultCode><FaultString></FaultString></FaultStruct>` +
		`<StartTime>` + now + `</StartTime>` +
		`<CompleteTime>` + now + `</CompleteTime>` +
		`</cwmp:TransferComplete>`
	return wrap(msgID, inner)
}

// atcEnvelope announces the file the device just uploaded on its own — a routine
// VendorLog (Log_*.gz). targetFile/size reflect the actual upload rather than a
// placeholder, so the agent's CWMP trace matches what really landed on FTP.
func atcEnvelope(msgID, targetFile string, size int) string {
	now := time.Now().UTC().Format(time.RFC3339)
	inner := `<cwmp:AutonomousTransferComplete>` +
		`<AnnounceURL></AnnounceURL><TransferURL>ftp://192.168.8.100/</TransferURL>` +
		`<IsDownload>0</IsDownload><FileType>4 Vendor Log File</FileType><FileSize>` + fmt.Sprintf("%d", size) + `</FileSize>` +
		`<TargetFileName>` + targetFile + `</TargetFileName>` +
		`<FaultStruct><FaultCode>0</FaultCode><FaultString></FaultString></FaultStruct>` +
		`<StartTime>` + now + `</StartTime>` +
		`<CompleteTime>` + now + `</CompleteTime>` +
		`</cwmp:AutonomousTransferComplete>`
	return wrap(msgID, inner)
}

// gpvResponseEnvelope answers a GetParameterValues with each path's manifest
// xsi:type (not a blanket xsd:string regardless of the real type).
func gpvResponseEnvelope(msgID string, pairs [][2]string, m *manifest) string {
	var pl strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&pl, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="%s">%s</Value></ParameterValueStruct>`, p[0], m.xsdType(p[0]), xmlEscape(p[1]))
	}
	inner := fmt.Sprintf(`<cwmp:GetParameterValuesResponse><ParameterList soap:arrayType="cwmp:ParameterValueStruct[%d]">%s</ParameterList></cwmp:GetParameterValuesResponse>`, len(pairs), pl.String())
	return wrap(msgID, inner)
}

func spvResponseEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`)
}

// gpnResponseEnvelope answers the first-contact writability walk, scoped to the
// requested ParameterPath (CWMP GetParameterNames semantics — the old stub
// ignored it and dumped the whole store). The managed snapshot paths are
// writable; identity/status paths are not. The agent walks "Device." (root,
// see session.go), so this legitimately returns the full 20,260-param tree —
// the agent's 16 MB request cap (server.go) is sized for exactly that.
func gpnResponseEnvelope(msgID string, dev *device, prefix string) string {
	writable := map[string]bool{
		refSignalPowerPath: true,
		"Device.ManagementServer.PeriodicInformInterval": true,
	}
	dev.mu.Lock()
	names := make([]string, 0, len(dev.params))
	for p := range dev.params {
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		names = append(names, p)
	}
	dev.mu.Unlock()
	var pl strings.Builder
	for _, p := range names {
		w := "0"
		if writable[p] || strings.Contains(p, ".CellConfig.") {
			w = "1"
		}
		fmt.Fprintf(&pl, `<ParameterInfoStruct><Name>%s</Name><Writable>%s</Writable></ParameterInfoStruct>`, p, w)
	}
	inner := fmt.Sprintf(`<cwmp:GetParameterNamesResponse><ParameterList soap:arrayType="cwmp:ParameterInfoStruct[%d]">%s</ParameterList></cwmp:GetParameterNamesResponse>`, len(names), pl.String())
	return wrap(msgID, inner)
}

func rebootResponseEnvelope(msgID string) string {
	return wrap(msgID, `<cwmp:RebootResponse></cwmp:RebootResponse>`)
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
