-- +migrate Up

UPDATE `group_category` SET `name` = '__default__' WHERE `is_default` = 1 AND `name` IN ('未分类', '默认分组');

-- +migrate Down

UPDATE `group_category` SET `name` = '默认分组' WHERE `is_default` = 1 AND `name` = '__default__';
