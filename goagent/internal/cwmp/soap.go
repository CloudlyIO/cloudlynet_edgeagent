package cwmp

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// soapOpen / soapClose wrap a marshalled CWMP body in the SOAP envelope the
// NanoLink expects. The cwmp / xsi / xsd prefixes are declared here so the
// rewritten cwmp:* root elements (and any xsi:type on Values) resolve.
const soapOpen = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-2">
<soapenv:Header><cwmp:ID soapenv:mustUnderstand="1">%s</cwmp:ID></soapenv:Header>
<soapenv:Body>`

const soapClose = `</soapenv:Body></soapenv:Envelope>`

// cwmpElement maps an ACS→CPE response/request struct to its cwmp:* element name.
func cwmpElement(v interface{}) string {
	switch v.(type) {
	case *InformResponse:
		return "cwmp:InformResponse"
	case *GetRPCMethodsResponse:
		return "cwmp:GetRPCMethodsResponse"
	case *TransferCompleteResponse:
		return "cwmp:TransferCompleteResponse"
	case *AutonomousTransferCompleteResponse:
		return "cwmp:AutonomousTransferCompleteResponse"
	case *GetParameterValues:
		return "cwmp:GetParameterValues"
	case *SetParameterValues:
		return "cwmp:SetParameterValues"
	case *GetParameterNames:
		return "cwmp:GetParameterNames"
	case *Reboot:
		return "cwmp:Reboot"
	}
	return ""
}

// DecodeEnvelope parses an inbound SOAP envelope. Strict=false tolerates the
// device's namespace-prefix quirks (elements match by local name), which is why
// a cwmp:/soapenv: prefixed body decodes into the un-prefixed struct tags.
func DecodeEnvelope(body []byte) (*Envelope, error) {
	var env Envelope
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("soap decode: %w", err)
	}
	return &env, nil
}

// EncodeResponse marshals an ACS→CPE body and wraps it in the SOAP envelope,
// renaming the marshalled root to its cwmp:* element (Go's encoding/xml does
// not prefix output element names natively — see rewriteRoot).
func EncodeResponse(msgID string, body interface{}) ([]byte, error) {
	el := cwmpElement(body)
	if el == "" {
		return nil, fmt.Errorf("cwmp: no element mapping for %T", body)
	}
	inner, err := xml.Marshal(body)
	if err != nil {
		return nil, err
	}
	wrapped := rewriteRoot(inner, el)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, soapOpen, msgID)
	buf.Write(wrapped)
	buf.WriteString(soapClose)
	return buf.Bytes(), nil
}

// rewriteRoot renames the root element of a marshalled fragment to el. Go
// marshals a struct with its Go type name as the root (e.g. "<InformResponse>…
// </InformResponse>" or "<AutonomousTransferCompleteResponse></…>"); we rename
// both the opening and closing root tags to the namespaced cwmp:* form. Nested
// child elements keep their own names (their tags never equal the root's).
func rewriteRoot(inner []byte, el string) []byte {
	s := inner
	if len(s) == 0 || s[0] != '<' {
		return inner
	}
	// Root local name runs from index 1 until the first delimiter.
	j := 1
	for j < len(s) && s[j] != '>' && s[j] != ' ' && s[j] != '/' {
		j++
	}
	local := string(s[1:j])
	if local == "" {
		return inner
	}
	// Opening tag: "<local" -> "<el" (prefix match preserves any attributes).
	s = bytes.Replace(s, []byte("<"+local), []byte("<"+el), 1)
	// Closing tag: "</local>" -> "</el>".
	s = bytes.Replace(s, []byte("</"+local+">"), []byte("</"+el+">"), 1)
	return s
}
