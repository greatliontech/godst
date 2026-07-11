# Graceful EOF can strand a concurrent connection reader

Lands: when persistent FIN/EOF state wakes every blocked reader that can make
progress

## Gap

Severity H. `dstStream.closeWrite` puts one token into a capacity-one ready
channel. When multiple goroutines are blocked in `dstWireEnd.read`, one reader
consumes the token and returns EOF, but the EOF path does not re-signal the
persistent condition. Another legal concurrent reader can remain blocked
forever, including after the first reader consumes the final buffered bytes.

## Required outcome

All readers blocked on a connection whose peer has gracefully closed eventually
observe data or EOF without a new external event. A regression parks two
readers and covers both empty-close and final-buffer-drained forms.
