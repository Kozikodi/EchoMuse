/* Residual echo suppression: the speexdsp preprocessor, as its own
 * translation unit because it shares helper names with mdf.c, which aec.go
 * compiles into its own. Vendored unmodified from SpeexDSP-1.2.1, the tag
 * mdf.c came from. See Canceller.SetResidual. */
#include "src/filterbank.c"
#include "src/preprocess.c"
