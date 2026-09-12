package agent

import "testing"

func TestEventBrokerCoalescesWakeupsAndUnsubscribes(t *testing.T) {
	broker := newEventBroker()
	wake, unsubscribe := broker.subscribe("turn_1")
	broker.notify("turn_1")
	broker.notify("turn_1")
	select {
	case <-wake:
	default:
		t.Fatal("subscriber was not notified")
	}
	select {
	case <-wake:
		t.Fatal("wakeups should coalesce because readers replay from SQLite")
	default:
	}
	unsubscribe()
	broker.notify("turn_1")
}
