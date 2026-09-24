// Package libiec61850 builds the vendored MZ Automation libiec61850 v1.6.2.
// Source: https://github.com/mz-automation/libiec61850/tree/v1.6.2
// License: GPL-3.0-or-later; see COPYING. Upstream sources are unmodified.
package libiec61850

/*
#cgo CFLAGS: -std=gnu99 -w -I${SRCDIR}/config -I${SRCDIR}/hal/inc -I${SRCDIR}/src/common/inc -I${SRCDIR}/src/mms/inc -I${SRCDIR}/src/mms/inc_private -I${SRCDIR}/src/mms/iso_mms/asn1c -I${SRCDIR}/src/iec61850/inc -I${SRCDIR}/src/iec61850/inc_private -I${SRCDIR}/src/goose -I${SRCDIR}/src/sampled_values -I${SRCDIR}/src/logging -I${SRCDIR}/src/r_session -I${SRCDIR}/src/tls
#cgo LDFLAGS: -lpthread
*/
import "C"
