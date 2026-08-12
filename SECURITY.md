# Security policy

## Supported versions

Security fixes are provided for the latest published release. Fleet operators
should pin the release version and minisign public key distributed through the
EduKod School Console and update schools after validating each new release.

## Reporting a vulnerability

Please report suspected vulnerabilities privately through GitHub's
**Security > Report a vulnerability** flow for this repository. Include the
affected version, impact, reproduction steps, and any relevant logs with
credentials and student data removed.

Do not open a public issue for an unpatched vulnerability. We will acknowledge
a report, investigate it, and coordinate disclosure after a fix is available.

## Release verification

EduKod publishes `SHA256SUMS`, `SHA256SUMS.minisig`, and release packages. Obtain
the minisign public key independently from EduKod Admin or the School Console,
verify the checksum manifest, and only then verify the selected package hash.
