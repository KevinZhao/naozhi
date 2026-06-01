package server

// APIVersion is the machine-readable REST contract version for the /api/*
// surface. It is mirrored both in the authenticated /health body
// (healthAuthSection.APIVersion) and, for any future header stamping, as the
// X-Naozhi-API-Version value. RNEW-ARCH-401 (#425): exposing a single,
// explicit version constant gives external consumers a break signal they can
// pin against instead of sniffing undocumented response shapes.
//
// Bump this when the /api/* contract changes in a backwards-incompatible way.
const APIVersion = "1"
