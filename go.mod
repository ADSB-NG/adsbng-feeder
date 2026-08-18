// adsbng-feeder — contributor-facing ADS-B Beast forwarder.
//
// DEPENDENCY POLICY: this module MUST have zero external dependencies.
// Everything here runs on a contributor's machine and is fully inspectable by
// them, so the smaller and more auditable the tree, the better. If you are
// tempted to add a require line, write ~40 lines of stdlib instead.
module github.com/adsbng/adsbng-feeder

go 1.24
