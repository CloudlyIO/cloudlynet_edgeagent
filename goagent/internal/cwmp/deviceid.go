package cwmp

import "strings"

// CanonicalID builds the canonical device id from the Inform DeviceID,
// percent-encoding content hyphens inside each component (so ProductClass
// "ENB-N03002-B3" -> "ENB%2DN03002%2DB3"). This is the ONE id used everywhere:
// SQLite keys, cloud cwmp_id mapping, dashboard. It MUST byte-match the id the
// previous ACS stored for this device, or the cloud re-onboards it as new.
//
//	CanonicalID("8C1F64", "ENB-N03002-B3", "2205609999")
//	  == "8C1F64-ENB%2DN03002%2DB3-2205609999"
func CanonicalID(oui, productClass, serial string) string {
	enc := func(s string) string { return strings.ReplaceAll(s, "-", "%2D") }
	return enc(oui) + "-" + enc(productClass) + "-" + enc(serial)
}
