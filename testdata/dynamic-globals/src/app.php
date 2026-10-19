<?php

$t = new Cart();
$t->add('top-level');

function rebind(string $name): void
{
    $GLOBALS[$name] = 1;
}

function local(): void
{
    $x = new Cart();
    $x->add('in-function');
}
