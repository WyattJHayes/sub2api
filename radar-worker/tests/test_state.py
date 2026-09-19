from uuid import uuid4

from sub2api_radar.state import AtomicStateStore, LocalState, StateRecord


def test_state_store_writes_atomically_and_round_trips(tmp_path) -> None:
    assignment_id = uuid4()
    store = AtomicStateStore(tmp_path)
    record = StateRecord(
        assignment_id,
        LocalState.EVIDENCE_READY,
        "idem",
        {"sha256": "abc"},
        "lease-token",
        7,
    )
    store.save(record)
    assert store.load(assignment_id) == record
    assert list(tmp_path.glob("*.tmp")) == []


def test_state_store_lists_and_deletes(tmp_path) -> None:
    assignment_id = uuid4()
    store = AtomicStateStore(tmp_path)
    store.save(StateRecord(assignment_id, LocalState.CLAIMED, "idem"))
    assert [item.assignment_id for item in store.list_records()] == [assignment_id]
    store.delete(assignment_id)
    assert store.load(assignment_id) is None
