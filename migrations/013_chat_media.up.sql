ALTER TABLE chat_messages ADD COLUMN media_url  TEXT         DEFAULT NULL;
ALTER TABLE chat_messages ADD COLUMN media_type VARCHAR(20)  DEFAULT NULL;
