-- Introduce a reverse index of group membership alongside the authoritative
-- repositories.members JSON array. The members column is still written by every
-- binary and remains the source for a repository's own member list; this relation
-- is populated so a later release can rebuild it, cut the inbound-membership check
-- over to an indexed lookup, and enforce it with a member foreign key.
--
-- This release neither reads the relation for any decision nor enforces a member
-- foreign key: during a rolling deployment an old binary writes only the JSON
-- column and cannot maintain the relation, so a member ON DELETE RESTRICT here
-- would block valid deletions against stale rows, and reading it for the deletion
-- check could act on a diverged list. Only the group foreign key is enforced, and
-- only to cascade the auxiliary rows when a group is removed.
CREATE TABLE repository_members (
    group_name  TEXT NOT NULL REFERENCES repositories(name) ON DELETE CASCADE,
    member_name TEXT NOT NULL,
    position    INTEGER NOT NULL,
    PRIMARY KEY (group_name, member_name)
);

CREATE INDEX idx_repository_members_member ON repository_members(member_name);

-- A historical member list may legitimately repeat a name (member validation does
-- not reject duplicates), while (group_name, member_name) is a set. Deduplicate by
-- grouping and keep the first occurrence's position.
INSERT INTO repository_members (group_name, member_name, position)
SELECT r.name, elem.value, MIN(elem.ordinality - 1)
FROM repositories r
CROSS JOIN LATERAL json_array_elements_text(r.members::json)
    WITH ORDINALITY AS elem(value, ordinality)
WHERE r.type = 'group'
GROUP BY r.name, elem.value;
