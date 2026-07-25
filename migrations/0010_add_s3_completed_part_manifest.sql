CREATE TABLE tg_s3_completed_part_tab (
    file_id INTEGER NOT NULL CHECK (file_id > 0),
    part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    part_size INTEGER NOT NULL CHECK (
        part_size >= 0 AND part_size <= 5368709120
    ),
    checksum_state TEXT NOT NULL
        CHECK (checksum_state IN ('available', 'unavailable')),
    checksum_algorithm TEXT NOT NULL DEFAULT ''
        CHECK (
            checksum_algorithm IN (
                '',
                'CRC32', 'CRC32C', 'CRC64NVME', 'SHA1', 'SHA256',
                'MD5', 'SHA512', 'XXHASH64', 'XXHASH3', 'XXHASH128'
            )
        ),
    checksum_value TEXT NOT NULL DEFAULT '',
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    PRIMARY KEY (file_id, part_number),
    CHECK (
        (
            checksum_state = 'unavailable'
            AND checksum_algorithm = ''
            AND checksum_value = ''
        )
        OR
        (
            checksum_state = 'available'
            AND checksum_algorithm != ''
            AND checksum_value != ''
        )
    )
);

INSERT INTO tg_s3_completed_part_tab (
    file_id,
    part_number,
    part_size,
    checksum_state,
    checksum_algorithm,
    checksum_value,
    ctime,
    mtime
)
SELECT
    file_id,
    segment_index + 1,
    segment_size,
    'unavailable',
    '',
    '',
    ctime,
    mtime
FROM tg_s3_file_segment_tab;

WITH ranked_selected_part AS (
    SELECT
        upload.result_file_id AS file_id,
        ROW_NUMBER() OVER (
            PARTITION BY upload.upload_id
            ORDER BY part.part_number
        ) AS final_part_number,
        upload.checksum_algorithm AS checksum_algorithm,
        part.checksum_value AS checksum_value
    FROM tg_s3_multipart_upload_tab AS upload
    JOIN tg_s3_multipart_part_tab AS part
      ON part.upload_id = upload.upload_id
    WHERE upload.upload_state = 'completed'
      AND upload.result_file_id > 0
      AND upload.checksum_algorithm != ''
      AND part.part_state = 'selected'
      AND part.checksum_value != ''
)
UPDATE tg_s3_completed_part_tab
SET
    checksum_state = 'available',
    checksum_algorithm = (
        SELECT ranked.checksum_algorithm
        FROM ranked_selected_part AS ranked
        WHERE ranked.file_id = tg_s3_completed_part_tab.file_id
          AND ranked.final_part_number = tg_s3_completed_part_tab.part_number
    ),
    checksum_value = (
        SELECT ranked.checksum_value
        FROM ranked_selected_part AS ranked
        WHERE ranked.file_id = tg_s3_completed_part_tab.file_id
          AND ranked.final_part_number = tg_s3_completed_part_tab.part_number
    )
WHERE EXISTS (
    SELECT 1
    FROM ranked_selected_part AS ranked
    WHERE ranked.file_id = tg_s3_completed_part_tab.file_id
      AND ranked.final_part_number = tg_s3_completed_part_tab.part_number
);
