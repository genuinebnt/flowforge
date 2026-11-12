// Package vm is the Flowforge virtual machine (spec 8.1, 8.2, 8.6, 8.7):
// a stack machine that runs a verified bytecode module as a pure function
// of its inputs and the recorded results of its effects. It never reads
// clocks, randomness, the environment or the network.
//
// Life of a run:
//
//	v, _ := vm.New(prog, vm.Config{})      // prog from bytecode.Verify
//	_ = v.Start(inputs)                     // vm.DecodeInputs for JSON
//	out, _ := v.Run(fuel)
//	for {
//		switch o := out.(type) {
//		case vm.Suspended:  // perform or replay o (path, kind, args)
//			out, _ = v.Resume(result, fuel)
//		case vm.OutOfFuel:  // resumable
//			out, _ = v.Run(fuel)
//		case vm.Completed, vm.Failed:
//			return
//		}
//	}
//
// Effects are numbered per VM in program order (Suspended.Seq) and named by
// hierarchical paths: "7" for the root run, "7/3/0" for the first effect of
// task 3 of the par_map that was effect 7 (ParMap.NewTask, ParMap.Join).
// Compensations registered with PUSH_COMP are returned by
// VM.Compensations in the order to run them (reverse registration); the
// engine runs each with NewChild. Snapshot and Restore serialize the whole
// state as deterministic CBOR; EncodeArgs and ArgsHash give the canonical
// effect-argument bytes the engine logs and compares during replay.
// RunWithEffects, Drive, ScriptedEffects and Replayer are the in-memory
// drivers used by tests and by the differential tree-walker test.
package vm
