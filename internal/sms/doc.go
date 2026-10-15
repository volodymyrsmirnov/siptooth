// Package sms is the MAP (Message Access Profile) client side of SMS
// forwarding (spec 17): the MNS event report parser, the bMessage parser and
// the vCard subset it needs (spec 17.6).
//
// Parsers never put input text in error values or logs: message bodies, names
// and numbers are personal data (spec 17.10).
package sms
