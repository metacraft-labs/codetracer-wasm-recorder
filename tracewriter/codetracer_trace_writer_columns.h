/*
 * codetracer_trace_writer_columns.h — wasm-recorder-local extension
 * adding the column-aware step prototypes (P6.3 / P6.4) that the
 * upstream `codetracer-trace-format-nim/include/codetracer_trace_writer.h`
 * has not yet been regenerated to surface.  The actual symbols are
 * exported by the Nim FFI's static library
 * (`libcodetracer_trace_writer.a`) — this file just hands cgo the C
 * prototypes so the call sites in `ctfs_writer.go` compile.
 *
 * Recorders that capture column information (Python `co_positions`,
 * DWARF column extraction, Cairo source maps, etc.) call
 * `trace_writer_enable_column_aware_steps` *before* any step is
 * registered, then `trace_writer_register_delta_column` for every
 * column-only move.  See the trace-format spec at
 * `codetracer-trace-format-spec/trace-events.md` §"Column Encoding —
 * `DeltaColumn` (chosen)" and §"Reader Behaviour and Back-Compat" for
 * the full contract.
 *
 * `trace_writer_register_path_with_line_lengths` writes a paths.dat
 * Layout A record (per-line byte counts) so the column-aware reader
 * can decode `DeltaColumn` events back to (line, column) pairs.  Pass
 * `line_count = 0` and `line_lengths = NULL` when the per-line data
 * isn't available — column resolution then falls back to surfacing
 * `None` at read time.  Outside column-aware mode the `line_lengths`
 * argument is ignored and the legacy paths.dat format is preserved
 * byte-for-byte.
 */

#ifndef CODETRACER_TRACE_WRITER_COLUMNS_H
#define CODETRACER_TRACE_WRITER_COLUMNS_H

#include "codetracer_trace_writer.h"

#ifdef __cplusplus
extern "C" {
#endif

void trace_writer_enable_column_aware_steps(trace_writer_t handle);
void trace_writer_register_delta_column(trace_writer_t handle, int64_t column_delta);
int  trace_writer_register_path_with_line_lengths(trace_writer_t handle,
                                                  const char* path,
                                                  int line_count,
                                                  const uint32_t* line_lengths);

#ifdef __cplusplus
}
#endif

#endif /* CODETRACER_TRACE_WRITER_COLUMNS_H */
