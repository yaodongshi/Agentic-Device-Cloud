"""In-memory A2A task store used only by unit tests."""

from __future__ import annotations

from app.a2a import (
    A2ATask,
    IdempotencyConflictError,
    TaskNotFoundError,
    TaskState,
    VersionConflictError,
)


class MemoryTaskStore:
    def __init__(self) -> None:
        self.tasks: dict[tuple[str, str], A2ATask] = {}
        self.idempotency: dict[tuple[str, str, str], tuple[str, str]] = {}

    async def start(self) -> None:
        return None

    async def close(self) -> None:
        return None

    async def ready(self) -> bool:
        return True

    async def create(self, task: A2ATask, idempotency_key: str, request_hash: str) -> A2ATask:
        key = (task.tenant_id, task.application_id, idempotency_key)
        existing = self.idempotency.get(key)
        if existing is not None:
            task_id, existing_hash = existing
            if existing_hash != request_hash:
                raise IdempotencyConflictError
            return self.tasks[(task.tenant_id, task_id)].model_copy(deep=True)
        self.tasks[(task.tenant_id, task.task_id)] = task.model_copy(deep=True)
        self.idempotency[key] = (task.task_id, request_hash)
        return task.model_copy(deep=True)

    async def list(self, tenant_id: str) -> list[A2ATask]:
        return [
            task.model_copy(deep=True)
            for (tenant, _task_id), task in self.tasks.items()
            if tenant == tenant_id
        ]

    async def get(self, tenant_id: str, task_id: str) -> A2ATask:
        task = self.tasks.get((tenant_id, task_id))
        if task is None:
            raise TaskNotFoundError
        return task.model_copy(deep=True)

    async def transition(
        self,
        tenant_id: str,
        task_id: str,
        expected_state: TaskState,
        expected_version: int,
        new_state: TaskState,
        message: str,
    ) -> A2ATask:
        task = await self.get(tenant_id, task_id)
        if task.state != expected_state or task.version != expected_version:
            raise VersionConflictError
        task.state = new_state
        task.version += 1
        task.message = message
        self.tasks[(tenant_id, task_id)] = task
        return task.model_copy(deep=True)
