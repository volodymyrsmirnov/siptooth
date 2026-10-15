// Package sms is the MAP (Message Access Profile) side of SMS forwarding
// (spec 17): the MNS event report parser, the bMessage parser and the vCard
// subset it needs (spec 17.6), the MAS client and MNS server over OBEX, and
// the Service that runs the MAP session lifecycle, retries, dedup and fetch
// queue (spec 17.7).
//
// Nothing here puts input text in error values or logs: message bodies, names
// and numbers are personal data (spec 17.10).
package sms
