package plugin

// this file primarily contains the type resourceTransition[T], for handling a number of operations
// on resources, and pretty-formatting summaries of the operations. There are also other, unrelated
// methods to perform similar functionality.
//
// resourceTransitions are created with the collectResourceTransition function.
//
// Handling requested resources from the autoscaler-agent is done with the handleRequested method,
// and changes from VM deletion are handled by handleDeleted.

import (
	"errors"
	"fmt"

	"golang.org/x/exp/constraints"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type resourceTransition[T constraints.Unsigned] struct {
	node    *nodeResourceState[T]
	oldNode struct {
		reserved             T
		buffer               T
		capacityPressure     T
		pressureAccountedFor T
	}
	pod    *podResourceState[T]
	oldPod struct {
		reserved         T
		buffer           T
		capacityPressure T
	}
}

func collectResourceTransition[T constraints.Unsigned](
	node *nodeResourceState[T],
	pod *podResourceState[T],
) resourceTransition[T] {
	return resourceTransition[T]{
		node: node,
		oldNode: struct {
			reserved             T
			buffer               T
			capacityPressure     T
			pressureAccountedFor T
		}{
			reserved:             node.Reserved,
			buffer:               node.Buffer,
			capacityPressure:     node.CapacityPressure,
			pressureAccountedFor: node.PressureAccountedFor,
		},
		pod: pod,
		oldPod: struct {
			reserved         T
			buffer           T
			capacityPressure T
		}{
			reserved:         pod.Reserved,
			buffer:           pod.Buffer,
			capacityPressure: pod.CapacityPressure,
		},
	}
}

// handleRequested updates r.pod and r.node with changes to match the requested resources, within
// what's possible given the remaining resources.
//
// A pretty-formatted summary of the outcome is returned as the verdict, for logging.
func (r resourceTransition[T]) handleRequested(requested T, startingMigration bool) (verdict string) {
	totalReservable := r.node.Total - r.node.System
	// note: it's possible to temporarily have reserved > totalReservable, after loading state or
	// config change; we have to use SaturatingSub here to account for that.
	remainingReservable := util.SaturatingSub(totalReservable, r.oldNode.reserved)

	// Note: The correctness of this function depends on the autoscaler-agents and previous
	// scheduler being well-behaved. This function will fail to prevent overcommitting when:
	//
	//  1. A previous scheduler allowed more than it should have, and the autoscaler-agents are
	//     sticking to those resource levels; or
	//  2. autoscaler-agents are making an initial communication that requests an increase from
	//     their current resource allocation
	//
	// So long as neither of the above happen, we should be ok, because we'll inevitably lower the
	// "buffered" amount until all pods for this node have contacted us, and the reserved amount
	// should then be back under r.node.total. It _may_ still be above totalReservable, but that's
	// expected to happen sometimes!

	if requested <= r.pod.Reserved {
		// Decrease "requests" are actually just notifications it's already happened
		r.node.Reserved -= r.pod.Reserved - requested
		r.pod.Reserved = requested
		// pressure is now zero, because the pod no longer wants to increase resources.
		r.pod.CapacityPressure = 0
		r.node.CapacityPressure -= r.oldPod.capacityPressure

		// use shared verdict below.

	} else if startingMigration /* implied: && requested > r.pod.reserved */ {
		// Can't increase during migrations.
		//
		// But we _will_ add the pod's request to the node's pressure, noting that its migration
		// will resolve it.
		r.pod.CapacityPressure = requested - r.pod.Reserved
		r.node.CapacityPressure = r.node.CapacityPressure + r.pod.CapacityPressure - r.oldPod.capacityPressure

		// note: we don't need to handle buffer here because migration is never started as the first
		// communication, so buffers will be zero already.
		if r.pod.Buffer != 0 {
			panic(errors.New("r.pod.buffer != 0"))
		}

		fmtString := "Denying increase %d -> %d because the pod is starting migration; " +
			"node capacityPressure %d -> %d (%d -> %d spoken for)"
		verdict = fmt.Sprintf(
			fmtString,
			// Denying increase %d -> %d because ...
			r.oldPod.reserved, requested,
			// node capacityPressure %d -> %d (%d -> %d spoken for)
			r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
		)
		return verdict
	} else /* typical "request for increase" */ {
		// The following comment was made 2022-11-28:
		//
		// Note: this function as currently written will actively cause the autoscaler-agent to use
		// resources that are uneven w.r.t. the number of compute units they represent.
		//
		// For example, we might have a request to go from 3CPU/3Gi -> 4CPU/4Gi but we only allow
		// 4CPU/3Gi, which would instead be 4 compute units of CPU but 3 compute units of memory.
		// When the autoscaler-agent receives the permit, it naively carries it out, giving itself a
		// resource allocation that isn't a multiple of compute units.
		//
		// This obviously isn't great. However, this *is* the most resilient solution, and it is
		// significantly simpler to implement, so it is the one I went with. As it currently stands,
		// the autoscaler-agent is still required to submit requests that are multiples of compute
		// units, so the system will *eventually* stabilize. This allows us to gracefully handle many
		// kinds of stressors. Handling the resources separately *from the scheduler's point of
		// view* makes it much, much easier to deal with.
		//
		// Please think carefully before changing this.

		increase := requested - r.pod.Reserved
		// Increases are bounded by what's left in the node
		maxIncrease := remainingReservable
		if increase > maxIncrease /* increases are bound by what's left in the node */ {
			r.pod.CapacityPressure = increase - maxIncrease
			// adjust node pressure accordingly. We can have old < new or new > old, so we shouldn't
			// directly += or -= (implicitly relying on overflow).
			r.node.CapacityPressure = r.node.CapacityPressure - r.oldPod.capacityPressure + r.pod.CapacityPressure
			increase = maxIncrease // cap at maxIncrease.
		} else {
			// If we're not capped by maxIncrease, relieve pressure coming from this pod
			r.node.CapacityPressure -= r.pod.CapacityPressure
			r.pod.CapacityPressure = 0
		}
		r.pod.Reserved += increase
		r.node.Reserved += increase

		// use shared verdict below.
	}

	fmtString := "Register %d%s -> %d%s (pressure %d -> %d); " +
		"node reserved %d -> %d (of %d), " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"

	var buffer string
	if r.pod.Buffer != 0 {
		buffer = fmt.Sprintf(" (buffer %d)", r.pod.Buffer)

		r.node.Buffer -= r.pod.Buffer
		r.pod.Buffer = 0
	}

	var wanted string
	if r.pod.Reserved != requested {
		wanted = fmt.Sprintf(" (wanted %d)", requested)
	}

	verdict = fmt.Sprintf(
		fmtString,
		// Register %d%s -> %d%s (pressure %d -> %d)
		r.oldPod.reserved, buffer, r.pod.Reserved, wanted, r.oldPod.capacityPressure, r.pod.CapacityPressure,
		// node reserved %d -> %d (of %d)
		r.oldNode.reserved, r.node.Reserved, totalReservable,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

// handleDeleted updates r.node with changes to match the removal of r.pod
//
// A pretty-formatted summary of the changes is returned as the verdict, for logging.
func (r resourceTransition[T]) handleDeleted(currentlyMigrating bool) (verdict string) {
	r.node.Reserved -= r.pod.Reserved
	r.node.CapacityPressure -= r.pod.CapacityPressure

	if currentlyMigrating {
		r.node.PressureAccountedFor -= r.pod.Reserved + r.pod.CapacityPressure
	}

	fmtString := "pod had %d; node reserved %d -> %d, " +
		"node capacityPressure %d -> %d (%d -> %d spoken for)"
	verdict = fmt.Sprintf(
		fmtString,
		// pod had %d; node reserved %d -> %d
		r.pod.Reserved, r.oldNode.reserved, r.node.Reserved,
		// node capacityPressure %d -> %d (%d -> %d spoken for)
		r.oldNode.capacityPressure, r.node.CapacityPressure, r.oldNode.pressureAccountedFor, r.node.PressureAccountedFor,
	)
	return verdict
}

// handleDeletedPod is kind of like handleDeleted, except that it returns both verdicts side by
// side instead of being generic over the resource
func handleDeletedPod(
	node *nodeState,
	pod podOtherResourceState,
	memSlotSize *resource.Quantity,
) (cpuVerdict string, memVerdict string) {

	oldRes := node.otherResources
	newRes := node.otherResources.subPod(memSlotSize, pod)
	node.otherResources = newRes

	oldNodeCpuReserved := node.vCPU.Reserved
	oldNodeMemReserved := node.memSlots.Reserved

	node.vCPU.Reserved = node.vCPU.Reserved - oldRes.ReservedCPU + newRes.ReservedCPU
	node.memSlots.Reserved = node.memSlots.Reserved - oldRes.ReservedMemSlots + newRes.ReservedMemSlots

	cpuVerdict = fmt.Sprintf(
		"pod had %v (raw), node otherPods (%v -> %v raw, %v margin, %d -> %d rounded), node reserved %d -> %d",
		&pod.RawCPU, &oldRes.RawCPU, &newRes.RawCPU, newRes.MarginCPU, oldRes.ReservedCPU, newRes.ReservedCPU,
		oldNodeCpuReserved, node.vCPU.Reserved,
	)
	memVerdict = fmt.Sprintf(
		"pod had %v (raw), node otherPods (%v -> %v raw, %v margin, %d -> %d slots), node reserved %d -> %d slots",
		&pod.RawMemory, &oldRes.RawMemory, &newRes.RawMemory, newRes.MarginMemory, oldRes.ReservedMemSlots, newRes.ReservedMemSlots,
		oldNodeMemReserved, node.memSlots.Reserved,
	)

	return
}
