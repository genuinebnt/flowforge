# conformance/vm

`conformance/vm/run/*.json` cases (DSL source, inputs, scripted effects,
expected outcome) arrive with the DSL-to-bytecode compiler; they drive
vm_008 (tree-walking evaluator versus VM, spec 25.3). Until then the VM is
tested with hand-assembled programs in `vm/*_test.go`, and the verifier
cases live in `conformance/bytecode/verify`.
