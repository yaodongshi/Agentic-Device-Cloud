"""PostgreSQL integration coverage for durable multi-instance A2A tasks."""

from __future__ import annotations

import asyncio
import os
import uuid

import asyncpg
import pytest
from app.a2a import (
    A2ATask,
    IdempotencyConflictError,
    PostgresTaskStore,
    TaskNotFoundError,
    TaskState,
    VersionConflictError,
)


def database_url() -> str:
    url = os.environ.get("ADC_MIGRATION_TEST_DATABASE_URL", "").strip()
    assert url, "ADC_MIGRATION_TEST_DATABASE_URL is required for PostgreSQL integration tests"
    return url


async def prepare_database() -> tuple[asyncpg.Connection, str, str, str]:
    connection = await asyncpg.connect(database_url())
    suffix = uuid.uuid4().hex
    tenant_id = str(uuid.uuid4())
    application_id = str(uuid.uuid4())
    user_id = str(uuid.uuid4())
    await connection.execute(
        "INSERT INTO adc_tenants(id,code,name) VALUES($1::uuid,$2,$3)",
        tenant_id,
        "a2a-it-" + suffix,
        "A2A Integration",
    )
    await connection.execute(
        """INSERT INTO adc_users(id,tenant_id,username,display_name)
        VALUES($1::uuid,$2::uuid,$3,'Integration Approver')""",
        user_id,
        tenant_id,
        "approver-" + suffix,
    )
    await connection.execute(
        """INSERT INTO adc_developer_applications(id,tenant_id,name,purpose)
        VALUES($1::uuid,$2::uuid,$3,'integration')""",
        application_id,
        tenant_id,
        "application-" + suffix,
    )
    return connection, tenant_id, application_id, user_id


def make_task(tenant_id: str, application_id: str) -> A2ATask:
    return A2ATask(
        task_id=str(uuid.uuid4()),
        tenant_id=tenant_id,
        application_id=application_id,
        state=TaskState.INPUT_REQUIRED,
        task_type="maintain",
        goal="replace spindle",
        devices=["cnc-01"],
        steps=[{"device": "cnc-01", "approval": "required"}],
        message="awaiting approval",
    )


@pytest.mark.parametrize("restart", [False, True])
def test_two_stores_and_restart_recover_authoritative_task(restart: bool) -> None:
    async def scenario() -> None:
        connection, tenant_id, application_id, _ = await prepare_database()
        first = PostgresTaskStore(database_url())
        second = PostgresTaskStore(database_url())
        try:
            await first.start()
            await second.start()
            task = make_task(tenant_id, application_id)
            created = await first.create(task, "recover-key", "a" * 64)
            if restart:
                await second.close()
                second = PostgresTaskStore(database_url())
                await second.start()
            recovered = await second.get(tenant_id, created.task_id)
            assert recovered == created
            assert recovered.state == TaskState.INPUT_REQUIRED
            assert recovered.version == 1
        finally:
            await first.close()
            await second.close()
            await connection.execute(
                "DELETE FROM adc_a2a_tasks WHERE tenant_id=$1::uuid", tenant_id
            )
            await connection.execute(
                "DELETE FROM adc_developer_applications WHERE tenant_id=$1::uuid", tenant_id
            )
            await connection.execute("DELETE FROM adc_users WHERE tenant_id=$1::uuid", tenant_id)
            await connection.execute("DELETE FROM adc_tenants WHERE id=$1::uuid", tenant_id)
            await connection.close()

    asyncio.run(scenario())


def test_postgres_idempotency_tenant_scope_and_cas() -> None:
    async def scenario() -> None:
        connection, tenant_id, application_id, _ = await prepare_database()
        store = PostgresTaskStore(database_url())
        try:
            await store.start()
            task = make_task(tenant_id, application_id)
            first = await store.create(task, "same-key", "b" * 64)
            duplicate = await store.create(
                make_task(tenant_id, application_id), "same-key", "b" * 64
            )
            assert duplicate.task_id == first.task_id
            with pytest.raises(IdempotencyConflictError):
                await store.create(make_task(tenant_id, application_id), "same-key", "c" * 64)
            other_application_id = str(uuid.uuid4())
            await connection.execute(
                """INSERT INTO adc_developer_applications(id,tenant_id,name,purpose)
                VALUES($1::uuid,$2::uuid,$3,'integration')""",
                other_application_id,
                tenant_id,
                "other-application-" + uuid.uuid4().hex,
            )
            other_application = await store.create(
                make_task(tenant_id, other_application_id), "same-key", "b" * 64
            )
            assert other_application.task_id != first.task_id
            with pytest.raises(TaskNotFoundError):
                await store.get(str(uuid.uuid4()), first.task_id)
            assert {task.task_id for task in await store.list(tenant_id)} == {
                first.task_id,
                other_application.task_id,
            }
            transitioned = await store.transition(
                tenant_id,
                first.task_id,
                TaskState.INPUT_REQUIRED,
                1,
                TaskState.WORKING,
                "approved; awaiting execution result",
            )
            assert transitioned.state == TaskState.WORKING
            assert transitioned.version == 2
            with pytest.raises(VersionConflictError):
                await store.transition(
                    tenant_id,
                    first.task_id,
                    TaskState.INPUT_REQUIRED,
                    1,
                    TaskState.REJECTED,
                    "late rejection",
                )
        finally:
            await store.close()
            await connection.execute(
                "DELETE FROM adc_a2a_tasks WHERE tenant_id=$1::uuid", tenant_id
            )
            await connection.execute(
                "DELETE FROM adc_developer_applications WHERE tenant_id=$1::uuid", tenant_id
            )
            await connection.execute("DELETE FROM adc_users WHERE tenant_id=$1::uuid", tenant_id)
            await connection.execute("DELETE FROM adc_tenants WHERE id=$1::uuid", tenant_id)
            await connection.close()

    asyncio.run(scenario())
