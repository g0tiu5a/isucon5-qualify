alter table entries add title varchar(191) not null default '';
UPDATE entries SET title=SUBSTRING_INDEX(body, '\n', 1);
