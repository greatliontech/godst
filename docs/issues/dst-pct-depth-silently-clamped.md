# PCT silently clamps requested depths above sixteen

Lands: when the public PCT depth range matches runtime capacity through support
or fail-loud validation

## Gap

Severity M. `runOptions` accepts every positive depth fitting `int32`, while
`dstSchedRootPCT` silently clamps depths above `dstPCTMaxDepth` to 16. A caller
requesting depth 17 receives only 15 change points, so the run searches a
shallower space than requested without reporting a cap.

## Required outcome

Accepted PCT depth produces the specified `d-1` change points. If the fixed
array remains bounded, the public entry rejects larger depths before invoking
the SUT and documents the bound.
