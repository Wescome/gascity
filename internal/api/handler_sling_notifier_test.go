package api

import "testing"

type controlDispatchPokeState struct {
	*fakeState
	controlPokes int
}

func (s *controlDispatchPokeState) PokeControlDispatcher() {
	s.controlPokes++
}

func TestAPINotifierPokeControlDispatchUsesDedicatedLane(t *testing.T) {
	state := &controlDispatchPokeState{fakeState: newFakeState(t)}
	notifier := apiNotifier{state: state}

	notifier.PokeControlDispatch(state.CityPath())

	if state.controlPokes != 1 {
		t.Fatalf("control dispatcher pokes = %d, want 1", state.controlPokes)
	}
	if state.pokeCount != 0 {
		t.Fatalf("generic pokes = %d, want 0", state.pokeCount)
	}
}

func TestAPINotifierPokeControlDispatchFallsBackToGenericPoke(t *testing.T) {
	state := newFakeState(t)
	notifier := apiNotifier{state: state}

	notifier.PokeControlDispatch(state.CityPath())

	if state.pokeCount != 1 {
		t.Fatalf("generic pokes = %d, want 1", state.pokeCount)
	}
}
