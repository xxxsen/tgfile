ALTER TABLE tg_s3_multipart_upload_tab
ADD COLUMN checksum_algorithm TEXT NOT NULL DEFAULT ''
    CHECK (
        checksum_algorithm IN (
            '', 'CRC32', 'CRC32C', 'CRC64NVME', 'SHA1', 'SHA256'
        )
    );

ALTER TABLE tg_s3_multipart_upload_tab
ADD COLUMN checksum_type TEXT NOT NULL DEFAULT ''
    CHECK (checksum_type IN ('', 'FULL_OBJECT', 'COMPOSITE'));

ALTER TABLE tg_s3_multipart_upload_tab
ADD COLUMN result_checksum_value TEXT NOT NULL DEFAULT '';

ALTER TABLE tg_s3_multipart_part_tab
ADD COLUMN checksum_value TEXT NOT NULL DEFAULT '';

ALTER TABLE tg_s3_object_metadata_tab
ADD COLUMN checksum_type TEXT NOT NULL DEFAULT ''
    CHECK (checksum_type IN ('', 'FULL_OBJECT', 'COMPOSITE'));

-- Object checksums saved before multipart checksum support always cover the
-- complete PutObject payload. This metadata-only backfill preserves that
-- existing meaning without reading or rewriting object content.
UPDATE tg_s3_object_metadata_tab
SET checksum_type = 'FULL_OBJECT'
WHERE request_checksum_algorithm != ''
  AND request_checksum_value != '';
