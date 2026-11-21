/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.nvidia.apikeys.persistance.repositories;

import static com.nvidia.apikeys.persistance.models.KeyModel.COLUMN_API_KEY_HASH;
import static com.nvidia.apikeys.persistance.models.KeyModel.COLUMN_DELETES_AT;
import static com.nvidia.apikeys.persistance.models.KeyModel.COLUMN_EXPIRES_AT;
import static com.nvidia.apikeys.persistance.models.KeyModel.COLUMN_KEY_DETAILS;
import static com.nvidia.apikeys.persistance.models.KeyModel.COLUMN_NCA_ID;
import static com.nvidia.apikeys.persistance.models.KeyModel.COLUMN_STATUS;
import static com.nvidia.apikeys.persistance.models.KeyModel.TABLE_NAME;

import com.datastax.oss.driver.api.core.DefaultConsistencyLevel;
import com.datastax.oss.driver.api.core.cql.ResultSet;
import com.nvidia.apikeys.persistance.models.KeyModel;
import java.util.Optional;
import org.springframework.data.cassandra.repository.Consistency;
import org.springframework.data.cassandra.repository.MapIdCassandraRepository;
import org.springframework.data.cassandra.repository.Query;
import org.springframework.data.repository.query.Param;
import org.springframework.stereotype.Repository;

@Repository
public interface KeyRepository extends MapIdCassandraRepository<KeyModel> {

    Optional<KeyModel> findByKeyHash(String hash);

    @Consistency(DefaultConsistencyLevel.LOCAL_QUORUM)
    @Query("SELECT " + COLUMN_API_KEY_HASH
            + ", " + COLUMN_NCA_ID
            + ", " + COLUMN_STATUS
            + ", " + COLUMN_EXPIRES_AT
            + ", " + COLUMN_DELETES_AT
            + ", " + COLUMN_KEY_DETAILS
            + ", WRITETIME(" + COLUMN_NCA_ID + ") AS wt_" + COLUMN_NCA_ID
            + ", WRITETIME(" + COLUMN_STATUS + ") AS wt_" + COLUMN_STATUS
            + ", WRITETIME(" + COLUMN_EXPIRES_AT + ") AS wt_" + COLUMN_EXPIRES_AT
            + ", WRITETIME(" + COLUMN_DELETES_AT + ") AS wt_" + COLUMN_DELETES_AT
            + ", WRITETIME(" + COLUMN_KEY_DETAILS + ") AS wt_" + COLUMN_KEY_DETAILS
            + " FROM " + TABLE_NAME
            + " WHERE " + COLUMN_API_KEY_HASH + "=:hash")
    ResultSet findMigrationRow(@Param("hash") String hash);

    @Consistency(DefaultConsistencyLevel.LOCAL_QUORUM)
    @Query(value = "UPDATE " + TABLE_NAME
            + " USING TIMESTAMP :timestamp SET " + COLUMN_NCA_ID + "=:ncaId"
            + " WHERE " + COLUMN_API_KEY_HASH + "=:hash",
            idempotent = Query.Idempotency.IDEMPOTENT)
    ResultSet setNcaIdAtTimestamp(@Param("hash") String hash, @Param("ncaId") String ncaId,
            @Param("timestamp") long timestamp);
}
