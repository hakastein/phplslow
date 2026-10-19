<?php

use Shop\Cart;

$x = new Cart();
$x->add('top-level');
(new Cart())->add('top-level-new');
$arrow = fn () => $x->add('top-level-arrow');
$closure = function () use ($x) {
    $x->add('top-level-closure');
};

function body(): void
{
    $x = new Cart();
    $x->add('function-body');
    (new Cart())->add('function-new');
    $arrow = fn () => $x->add('function-arrow');
    $closure = function () use ($x) {
        $x->add('function-closure');
    };
}
